// Package anthropic adapts the Anthropic Messages API to the provider
// port: plain net/http, the shared retry client and the shared SSE
// decoder, with no vendor SDK.
package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"

	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/internal/httpx"
)

// DefaultVersion is the anthropic-version header the API requires.
const DefaultVersion = "2023-06-01"

// Client is the Anthropic adapter.
type Client struct {
	base    string
	version string
	http    *httpx.Client
	creds   provider.CredentialSource
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the adapter at another host (tests, proxies).
func WithBaseURL(u string) Option { return func(c *Client) { c.base = strings.TrimSuffix(u, "/") } }

// WithHTTP replaces the shared retry client.
func WithHTTP(h *httpx.Client) Option { return func(c *Client) { c.http = h } }

// WithVersion overrides the anthropic-version header.
func WithVersion(v string) Option { return func(c *Client) { c.version = v } }

// New returns an Anthropic adapter that reads its API key from creds on
// every call, so a rotated key needs no new client.
func New(creds provider.CredentialSource, opts ...Option) *Client {
	c := &Client{
		base:    "https://api.anthropic.com",
		version: DefaultVersion,
		http:    httpx.New(),
		creds:   creds,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "anthropic" }

// Models implements provider.Provider with the catalogue of section 17's
// first adapters; pricing is list price in micro-USD per million tokens.
func (c *Client) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	return catalogue(), nil
}

// Capabilities implements provider.Provider.
func (c *Client) Capabilities(model string) (provider.Capabilities, error) {
	for _, m := range catalogue() {
		if m.ID == model {
			return m.Capabilities, nil
		}
	}
	return provider.Capabilities{}, fmt.Errorf("anthropic: unknown model %q", model)
}

func catalogue() []provider.ModelInfo {
	return []provider.ModelInfo{
		{
			ID: "claude-sonnet-4-5-20250929",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, Vision: true, PDF: true,
				Thinking: true, FileUpload: true, PromptCaching: true,
				ContextWindow: 200_000, MaxOutputTokens: 64_000,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000, CachedMicrosPerMTok: 300_000},
		},
		{
			ID: "claude-haiku-4-5-20251001",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, Vision: true, PDF: true,
				Thinking: true, FileUpload: true, PromptCaching: true,
				ContextWindow: 200_000, MaxOutputTokens: 64_000,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 5_000_000, CachedMicrosPerMTok: 100_000},
		},
		{
			ID: "claude-opus-4-1-20250805",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, Vision: true, PDF: true,
				Thinking: true, FileUpload: true, PromptCaching: true,
				ContextWindow: 200_000, MaxOutputTokens: 32_000,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 15_000_000, OutputMicrosPerMTok: 75_000_000, CachedMicrosPerMTok: 1_500_000},
		},
	}
}

// --- request mapping ---

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (c *Client) buildBody(ctx context.Context, req provider.Request, stream bool) (map[string]any, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("anthropic: request has no model")
	}
	caps, err := c.Capabilities(req.Model)
	if err != nil {
		return nil, err
	}
	if len(req.Tools) > 0 && !caps.NativeTools {
		return nil, fmt.Errorf("anthropic: model %q cannot take tools", req.Model)
	}

	body := map[string]any{"model": req.Model}
	maxOut := req.MaxOutputTokens
	if maxOut <= 0 {
		// The Messages API makes max_tokens mandatory.
		maxOut = caps.MaxOutputTokens
		if maxOut <= 0 {
			maxOut = 4096
		}
	}
	body["max_tokens"] = maxOut
	if stream {
		body["stream"] = true
	}
	if s := systemText(req.System); s != "" {
		body["system"] = s
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			input := json.RawMessage(`{}`)
			if len(t.Schema) > 0 {
				input = t.Schema
			}
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": input,
			})
		}
		body["tools"] = tools
	}

	messages := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		blocks, err := contentBlocks(ctx, m.Parts, caps)
		if err != nil {
			return nil, err
		}
		// Tool results ride inside a user message (section 10's mapping).
		role := "user"
		if m.Role == provider.AssistantRole {
			role = "assistant"
		}
		raw, err := json.Marshal(blocks)
		if err != nil {
			return nil, fmt.Errorf("anthropic: encode content: %w", err)
		}
		messages = append(messages, wireMessage{Role: role, Content: raw})
	}
	body["messages"] = messages
	return body, nil
}

func systemText(parts []provider.Part) string {
	var sb strings.Builder
	for _, p := range parts {
		if t, ok := p.(provider.TextPart); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

// contentBlocks maps provider parts onto Anthropic content blocks. An
// image or file with no source cannot be represented, so it is an error —
// never a silent drop.
func contentBlocks(ctx context.Context, parts []provider.Part, caps provider.Capabilities) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case provider.TextPart:
			out = append(out, map[string]any{"type": "text", "text": v.Text})
		case provider.ThinkingPart:
			b := map[string]any{"type": "thinking", "thinking": v.Text}
			if v.Signature != "" {
				b["signature"] = v.Signature
			}
			out = append(out, b)
		case provider.ImagePart:
			if !caps.Vision {
				return nil, fmt.Errorf("anthropic: model cannot take images")
			}
			b, err := mediaBlock(ctx, "image", v.Source, v.RemoteID)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		case provider.FilePart:
			b, err := mediaBlock(ctx, "document", v.Source, v.RemoteID)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		case provider.ToolCallPart:
			input := json.RawMessage(`{}`)
			if len(v.Arguments) > 0 {
				input = v.Arguments
			}
			out = append(out, map[string]any{"type": "tool_use", "id": v.CallID, "name": v.Name, "input": input})
		case provider.ToolResultPart:
			b := map[string]any{"type": "tool_result", "tool_use_id": v.CallID}
			if v.IsError {
				b["is_error"] = true
			}
			text := strings.TrimSpace(string(v.Output))
			if text == "" {
				text = "{}"
			}
			b["content"] = []map[string]any{{"type": "text", "text": text}}
			out = append(out, b)
		default:
			return nil, fmt.Errorf("anthropic: unsupported part %T", p)
		}
	}
	return out, nil
}

func mediaBlock(ctx context.Context, kind string, src provider.Source, remoteID string) (map[string]any, error) {
	switch {
	case remoteID != "":
		return map[string]any{"type": kind, "source": map[string]any{"type": "file", "file_id": remoteID}}, nil
	case src != nil:
		rc, err := src.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("anthropic: open %s source: %w", kind, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("anthropic: read %s source: %w", kind, err)
		}
		return map[string]any{"type": kind, "source": map[string]any{
			"type":       "base64",
			"media_type": src.MediaType(),
			"data":       base64.StdEncoding.EncodeToString(data),
		}}, nil
	default:
		return nil, fmt.Errorf("anthropic: %s part has neither a source nor an uploaded id", kind)
	}
}

// --- response mapping ---

type wireResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (c *Client) headers(ctx context.Context) (map[string]string, error) {
	key, err := c.creds.Credential(ctx, c.Name())
	if err != nil {
		return nil, fmt.Errorf("anthropic: credential: %w", err)
	}
	if key == "" {
		return nil, fmt.Errorf("anthropic: empty API key")
	}
	return map[string]string{
		"x-api-key":         key,
		"anthropic-version": c.version,
	}, nil
}

// Generate implements provider.Provider.
func (c *Client) Generate(ctx context.Context, req provider.Request) (provider.Response, error) {
	hdr, err := c.headers(ctx)
	if err != nil {
		return provider.Response{}, err
	}
	body, err := c.buildBody(ctx, req, false)
	if err != nil {
		return provider.Response{}, err
	}
	res, err := c.http.JSON(ctx, c.base+"/v1/messages", hdr, body)
	if err != nil {
		return provider.Response{}, err
	}
	if res.Status != http.StatusOK {
		return provider.Response{}, mapAPIError(res.Status, res.Body)
	}
	var wr wireResponse
	if err := json.Unmarshal(res.Body, &wr); err != nil {
		return provider.Response{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	msg, err := decodeContent(wr.Content)
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Response{
		Message:           msg,
		Usage:             decodeUsage(wr.Usage.InputTokens, wr.Usage.OutputTokens, wr.Usage.CacheReadInputTokens),
		Finish:            decodeFinish(wr.StopReason),
		Model:             orModel(wr.Model, req.Model),
		ProviderRequestID: res.Header.Get("request-id"),
	}, nil
}

func decodeUsage(in, out, cached int) provider.Usage {
	return provider.Usage{InputTokens: in, OutputTokens: out, CachedTokens: cached}
}

func decodeFinish(stop string) provider.FinishReason {
	switch stop {
	case "max_tokens":
		return provider.FinishLength
	case "tool_use":
		return provider.FinishToolCalls
	case "refusal":
		return provider.FinishContentFilter
	default: // end_turn, stop_sequence
		return provider.FinishStop
	}
}

func decodeContent(blocks []struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}) (provider.Message, error) {
	parts := make([]provider.Part, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, provider.TextPart{Text: b.Text})
		case "thinking":
			parts = append(parts, provider.ThinkingPart{Text: b.Thinking, Signature: b.Signature})
		case "tool_use":
			parts = append(parts, provider.ToolCallPart{CallID: b.ID, Name: b.Name, Arguments: b.Input})
		default:
			// A future block type must not be silently lost.
			parts = append(parts, provider.TextPart{Text: string(b.Input)})
		}
	}
	return provider.Message{Role: provider.AssistantRole, Parts: parts}, nil
}

func orModel(got, want string) string {
	if got == "" {
		return want
	}
	return got
}

type wireError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func mapAPIError(status int, body []byte) error {
	var we wireError
	_ = json.Unmarshal(body, &we)
	return &httpx.Error{Status: status, Type: we.Error.Type, Message: we.Error.Message}
}

// --- streaming ---

// Stream implements provider.Provider.
func (c *Client) Stream(ctx context.Context, req provider.Request) (provider.Stream, error) {
	hdr, err := c.headers(ctx)
	if err != nil {
		return nil, err
	}
	caps, err := c.Capabilities(req.Model)
	if err != nil {
		return nil, err
	}
	if !caps.Streaming {
		return nil, fmt.Errorf("anthropic: model %q cannot stream", req.Model)
	}
	body, err := c.buildBody(ctx, req, true)
	if err != nil {
		return nil, err
	}
	conn, err := c.http.Stream(ctx, c.base+"/v1/messages", hdr, body)
	if err != nil {
		if se, ok := err.(*httpx.StreamError); ok {
			return nil, mapAPIError(se.Status, se.Body)
		}
		return nil, err
	}
	return &stream{
		dec:    provider.NewSSEDecoder(conn.Body),
		body:   conn.Body,
		blocks: map[int]*streamBlock{},
	}, nil
}

type streamBlock struct {
	kind      string // text | thinking | tool_use
	text      []byte
	json      []byte
	thinking  []byte
	signature []byte
	name      string
	callID    string
}

type stream struct {
	dec     *provider.SSEDecoder
	body    io.ReadCloser
	blocks  map[int]*streamBlock
	pending []provider.Event
	usage   provider.Usage
	finish  provider.FinishReason
	stopped bool
}

// Recv implements provider.Stream.
func (s *stream) Recv() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.stopped {
			return provider.Event{}, io.EOF
		}
		ev, err := s.dec.Next()
		if err == io.EOF {
			return provider.Event{}, fmt.Errorf("anthropic: stream ended before message_stop")
		}
		if err != nil {
			return provider.Event{}, err
		}
		if err := s.mapFrame(ev); err != nil {
			return provider.Event{}, err
		}
	}
}

// Close implements provider.Stream.
func (s *stream) Close() error { return s.body.Close() }

func (s *stream) mapFrame(ev provider.SSEEvent) error {
	var frame struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(ev.Data, &frame); err != nil {
		return fmt.Errorf("anthropic: decode %s frame: %w", ev.Event, err)
	}
	switch frame.Type {
	case "error":
		var we wireError
		_ = json.Unmarshal(ev.Data, &we)
		return &httpx.Error{Status: 0, Type: we.Error.Type, Message: we.Error.Message}
	case "message_start":
		var start struct {
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		_ = json.Unmarshal(ev.Data, &start)
		s.usage.InputTokens = start.Message.Usage.InputTokens
		u := s.usage
		s.pending = append(s.pending, provider.Event{Kind: provider.UsageReported, Usage: &u})
	case "content_block_start":
		var start struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
		}
		_ = json.Unmarshal(ev.Data, &start)
		b := &streamBlock{kind: start.ContentBlock.Type}
		switch b.kind {
		case "text":
			b.text = []byte(start.ContentBlock.Text)
		case "tool_use":
			b.callID = start.ContentBlock.ID
			b.name = start.ContentBlock.Name
			if len(start.ContentBlock.Input) > 0 {
				b.json = start.ContentBlock.Input
			}
		}
		s.blocks[start.Index] = b
		s.pending = append(s.pending, provider.Event{Kind: provider.PartStarted, Index: start.Index})
	case "content_block_delta":
		var delta struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(ev.Data, &delta)
		b := s.blocks[delta.Index]
		if b == nil {
			b = &streamBlock{kind: "text"}
			s.blocks[delta.Index] = b
		}
		switch delta.Delta.Type {
		case "text_delta":
			b.text = append(b.text, delta.Delta.Text...)
			s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: delta.Index, Delta: delta.Delta.Text})
		case "input_json_delta":
			b.json = append(b.json, delta.Delta.PartialJSON...)
			s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: delta.Index, Delta: delta.Delta.PartialJSON})
		case "thinking_delta":
			b.thinking = append(b.thinking, delta.Delta.Thinking...)
			s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: delta.Index, Delta: delta.Delta.Thinking})
		case "signature_delta":
			b.signature = append(b.signature, delta.Delta.Signature...)
		}
	case "content_block_stop":
		var stop struct {
			Index int `json:"index"`
		}
		_ = json.Unmarshal(ev.Data, &stop)
		b := s.blocks[stop.Index]
		if b == nil {
			return fmt.Errorf("anthropic: block %d stopped before it started", stop.Index)
		}
		s.pending = append(s.pending, provider.Event{Kind: provider.PartDone, Index: stop.Index, Part: b.complete()})
	case "message_delta":
		var md struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(ev.Data, &md)
		s.finish = decodeFinish(md.Delta.StopReason)
		s.usage.OutputTokens = md.Usage.OutputTokens
		u := s.usage
		s.pending = append(s.pending, provider.Event{Kind: provider.UsageReported, Usage: &u})
	case "message_stop":
		s.stopped = true
		s.pending = append(s.pending, provider.Event{Kind: provider.Finished, Finish: s.finish})
	}
	return nil
}

func (b *streamBlock) complete() provider.Part {
	switch b.kind {
	case "thinking":
		return provider.ThinkingPart{Text: string(b.thinking), Signature: string(b.signature)}
	case "tool_use":
		args := b.json
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		return provider.ToolCallPart{CallID: b.callID, Name: b.name, Arguments: args}
	default:
		return provider.TextPart{Text: string(b.text)}
	}
}

// UploadFile implements provider.Provider with the Files API: a multipart
// POST returning the uploaded file's id.
func (c *Client) UploadFile(ctx context.Context, src provider.Source, name string) (provider.RemoteFile, error) {
	key, err := c.creds.Credential(ctx, c.Name())
	if err != nil {
		return provider.RemoteFile{}, fmt.Errorf("anthropic: credential: %w", err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(name)))
	if src != nil {
		hdr.Set("Content-Type", src.MediaType())
	}
	fw, err := w.CreatePart(hdr)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	if src != nil {
		rc, err := src.Open(ctx)
		if err != nil {
			return provider.RemoteFile{}, err
		}
		defer rc.Close()
		if _, err := io.Copy(fw, rc); err != nil {
			return provider.RemoteFile{}, err
		}
	}
	if err := w.Close(); err != nil {
		return provider.RemoteFile{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/files", &buf)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", c.version)

	resp, err := c.http.HTTP.Do(req)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return provider.RemoteFile{}, mapAPIError(resp.StatusCode, body)
	}
	var up struct {
		ID        string `json:"id"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &up); err != nil {
		return provider.RemoteFile{}, fmt.Errorf("anthropic: decode upload: %w", err)
	}
	out := provider.RemoteFile{ID: up.ID}
	if up.ExpiresAt != "" {
		if t, err := parseTime(up.ExpiresAt); err == nil {
			out.ExpiresAt = t
		}
	}
	return out, nil
}

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }
