// Package openai adapts the OpenAI Chat Completions API to the provider
// port: plain net/http, the shared retry client and the shared SSE
// decoder, with no vendor SDK.
package openai

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

	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/internal/httpx"
)

// Client is the OpenAI adapter. Its options also carry the small differences
// an OpenAI-compatible gateway needs (name, extra headers, model resolution),
// which is how the openrouter adapter reuses this wire code.
type Client struct {
	base         string
	name         string
	http         *httpx.Client
	creds        provider.CredentialSource
	extraHeaders map[string]string
	capabilities func(string) (provider.Capabilities, error)
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the adapter at another host (tests, proxies).
func WithBaseURL(u string) Option { return func(c *Client) { c.base = strings.TrimSuffix(u, "/") } }

// WithHTTP replaces the shared retry client.
func WithHTTP(h *httpx.Client) Option { return func(c *Client) { c.http = h } }

// WithName changes the provider name, which is what Name reports and what the
// credential source is asked for.
func WithName(name string) Option { return func(c *Client) { c.name = name } }

// WithHeaders adds headers to every request. Attribution headers such as
// HTTP-Referer and X-Title are the usual reason; Authorization is always set
// from the credential source and cannot be overridden here.
func WithHeaders(h map[string]string) Option {
	return func(c *Client) {
		for k, v := range h {
			c.extraHeaders[k] = v
		}
	}
}

// WithCapabilities resolves a model's capabilities through fn instead of the
// static catalogue. It lets a gateway adapter accept model IDs it learns at
// runtime; returning an error still fails the request fast.
func WithCapabilities(fn func(string) (provider.Capabilities, error)) Option {
	return func(c *Client) { c.capabilities = fn }
}

// New returns an OpenAI adapter that reads its API key from creds on every
// call, so a rotated key needs no new client.
func New(creds provider.CredentialSource, opts ...Option) *Client {
	c := &Client{
		base:         "https://api.openai.com/v1",
		name:         "openai",
		http:         httpx.New(),
		creds:        creds,
		extraHeaders: map[string]string{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return c.name }

// Models implements provider.Provider with the catalogue of section 17's
// first adapters; pricing is list price in micro-USD per million tokens.
func (c *Client) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	return catalogue(), nil
}

// Capabilities implements provider.Provider.
func (c *Client) Capabilities(model string) (provider.Capabilities, error) {
	if c.capabilities != nil {
		return c.capabilities(model)
	}
	for _, m := range catalogue() {
		if m.ID == model {
			return m.Capabilities, nil
		}
	}
	return provider.Capabilities{}, fmt.Errorf("openai: unknown model %q", model)
}

func catalogue() []provider.ModelInfo {
	return []provider.ModelInfo{
		{
			ID: "gpt-5",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   400_000,
				MaxOutputTokens: 128_000,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 1_250_000, OutputMicrosPerMTok: 10_000_000, CachedMicrosPerMTok: 125_000},
		},
		{
			ID: "gpt-5-mini",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   400_000,
				MaxOutputTokens: 128_000,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 250_000, OutputMicrosPerMTok: 2_000_000, CachedMicrosPerMTok: 25_000},
		},
		{
			ID: "gpt-4o",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, FileUpload: true, PromptCaching: true,
				ContextWindow:   128_000,
				MaxOutputTokens: 16_000,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 2_500_000, OutputMicrosPerMTok: 10_000_000, CachedMicrosPerMTok: 1_250_000},
		},
	}
}

// --- request mapping ---

type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

func (c *Client) buildBody(ctx context.Context, req provider.Request, stream bool) (map[string]any, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("openai: request has no model")
	}
	caps, err := c.Capabilities(req.Model)
	if err != nil {
		return nil, err
	}
	if len(req.Tools) > 0 && !caps.NativeTools {
		return nil, fmt.Errorf("openai: model %q cannot take tools", req.Model)
	}

	body := map[string]any{"model": req.Model}
	maxOut := req.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = caps.MaxOutputTokens
	}
	if maxOut > 0 {
		// max_completion_tokens is the current name and every catalogue
		// model accepts it.
		body["max_completion_tokens"] = maxOut
	}
	if stream {
		body["stream"] = true
		// Usage only arrives on the final chunk when it is asked for.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := json.RawMessage(`{}`)
			if len(t.Schema) > 0 {
				params = t.Schema
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		body["tools"] = tools
	}

	messages, err := wireMessages(ctx, req, caps)
	if err != nil {
		return nil, err
	}
	body["messages"] = messages
	return body, nil
}

// wireMessages flattens the transcript: system parts become one system
// message, tool results become tool messages, and an assistant message
// carries its tool calls alongside its content.
func wireMessages(ctx context.Context, req provider.Request, caps provider.Capabilities) ([]wireMessage, error) {
	var out []wireMessage
	if s := systemText(req.System); s != "" {
		out = append(out, wireMessage{Role: "system", Content: rawJSON(s)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case provider.ToolRole:
			for _, p := range m.Parts {
				res, ok := p.(provider.ToolResultPart)
				if !ok {
					return nil, fmt.Errorf("openai: tool message holds a %T", p)
				}
				text := strings.TrimSpace(string(res.Output))
				if text == "" {
					text = "{}"
				}
				out = append(out, wireMessage{Role: "tool", ToolCallID: res.CallID, Content: rawJSON(text)})
			}
		default:
			wm, err := contentMessage(ctx, m, caps)
			if err != nil {
				return nil, err
			}
			if wm != nil {
				out = append(out, *wm)
			}
		}
	}
	return out, nil
}

func contentMessage(ctx context.Context, m provider.Message, caps provider.Capabilities) (*wireMessage, error) {
	role := "user"
	if m.Role == provider.AssistantRole {
		role = "assistant"
	}
	var blocks []map[string]any
	var calls []map[string]any
	for _, p := range m.Parts {
		switch v := p.(type) {
		case provider.TextPart:
			blocks = append(blocks, map[string]any{"type": "text", "text": v.Text})
		case provider.ThinkingPart:
			// Chat Completions has no reasoning blocks (section 6's table:
			// thoughts are display material, not request material).
			continue
		case provider.ImagePart:
			if !caps.Vision {
				return nil, fmt.Errorf("openai: model cannot take images")
			}
			url, err := imageURL(ctx, v.Source, v.RemoteID)
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(url, "[") {
				blocks = append(blocks, map[string]any{"type": "text", "text": url})
			} else {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case provider.FilePart:
			// Chat Completions has no file blocks; a placeholder keeps the
			// reference visible instead of dropping it silently.
			name := v.Name
			if name == "" {
				name = v.RemoteID
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": "[file: " + name + "]"})
		case provider.ToolCallPart:
			args := "{}"
			if len(v.Arguments) > 0 {
				args = string(v.Arguments)
			}
			calls = append(calls, map[string]any{
				"id":   v.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      v.Name,
					"arguments": args,
				},
			})
		case provider.ToolResultPart:
			return nil, fmt.Errorf("openai: tool result outside a tool message")
		default:
			return nil, fmt.Errorf("openai: unsupported part %T", p)
		}
	}

	wm := &wireMessage{Role: role}
	if len(blocks) > 0 || role != "assistant" {
		raw, err := json.Marshal(blocks)
		if err != nil {
			return nil, fmt.Errorf("openai: encode content: %w", err)
		}
		wm.Content = raw
	}
	if len(calls) > 0 {
		raw, err := json.Marshal(calls)
		if err != nil {
			return nil, fmt.Errorf("openai: encode tool calls: %w", err)
		}
		wm.ToolCalls = raw
	}
	return wm, nil
}

func imageURL(ctx context.Context, src provider.Source, remoteID string) (string, error) {
	switch {
	case remoteID != "":
		// Chat Completions takes URLs and data URLs, not uploaded file ids.
		return "[image: " + remoteID + "]", nil
	case src != nil:
		rc, err := src.Open(ctx)
		if err != nil {
			return "", fmt.Errorf("openai: open image source: %w", err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return "", fmt.Errorf("openai: read image source: %w", err)
		}
		return "data:" + src.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	default:
		return "", fmt.Errorf("openai: image part has neither a source nor an uploaded id")
	}
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

func rawJSON(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

// --- response mapping ---

type wireResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (c *Client) headers(ctx context.Context) (map[string]string, error) {
	key, err := c.creds.Credential(ctx, c.Name())
	if err != nil {
		return nil, fmt.Errorf("openai: credential: %w", err)
	}
	if key == "" {
		return nil, fmt.Errorf("openai: empty API key")
	}
	hdr := make(map[string]string, len(c.extraHeaders)+1)
	for k, v := range c.extraHeaders {
		hdr[k] = v
	}
	hdr["Authorization"] = "Bearer " + key
	return hdr, nil
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
	res, err := c.http.JSON(ctx, c.base+"/chat/completions", hdr, body)
	if err != nil {
		return provider.Response{}, err
	}
	if res.Status != http.StatusOK {
		return provider.Response{}, mapAPIError(res.Status, res.Body)
	}
	var wr wireResponse
	if err := json.Unmarshal(res.Body, &wr); err != nil {
		return provider.Response{}, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(wr.Choices) == 0 {
		return provider.Response{}, fmt.Errorf("openai: response has no choices")
	}
	ch := wr.Choices[0]
	msg := provider.Message{Role: provider.AssistantRole}
	if ch.Message.Content != "" {
		msg.Parts = append(msg.Parts, provider.TextPart{Text: ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		msg.Parts = append(msg.Parts, provider.ToolCallPart{CallID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}
	return provider.Response{
		Message: msg,
		Usage: provider.Usage{
			InputTokens:     wr.Usage.PromptTokens,
			OutputTokens:    wr.Usage.CompletionTokens,
			CachedTokens:    wr.Usage.PromptTokensDetails.CachedTokens,
			ReasoningTokens: wr.Usage.CompletionTokensDetails.ReasoningTokens,
		},
		Finish:            decodeFinish(ch.FinishReason),
		Model:             orModel(wr.Model, req.Model),
		ProviderRequestID: res.Header.Get("x-request-id"),
	}, nil
}

func decodeFinish(stop string) provider.FinishReason {
	switch stop {
	case "length":
		return provider.FinishLength
	case "tool_calls":
		return provider.FinishToolCalls
	case "content_filter":
		return provider.FinishContentFilter
	default: // stop, null
		return provider.FinishStop
	}
}

func orModel(got, want string) string {
	if got == "" {
		return want
	}
	return got
}

type wireError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
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
		return nil, fmt.Errorf("openai: model %q cannot stream", req.Model)
	}
	body, err := c.buildBody(ctx, req, true)
	if err != nil {
		return nil, err
	}
	conn, err := c.http.Stream(ctx, c.base+"/chat/completions", hdr, body)
	if err != nil {
		if se, ok := err.(*httpx.StreamError); ok {
			return nil, mapAPIError(se.Status, se.Body)
		}
		return nil, err
	}
	return &stream{
		dec:   provider.NewSSEDecoder(conn.Body),
		body:  conn.Body,
		tools: map[int]*streamTool{},
	}, nil
}

type streamTool struct {
	id   string
	name string
	args []byte
}

type stream struct {
	dec      *provider.SSEDecoder
	body     io.ReadCloser
	tools    map[int]*streamTool
	toolList []int // vendor indexes in first-seen order
	pending  []provider.Event
	text     []byte
	textOn   bool
	usage    provider.Usage
	finish   provider.FinishReason
	stopped  bool
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
			return provider.Event{}, fmt.Errorf("openai: stream ended before [DONE]")
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

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (s *stream) mapFrame(ev provider.SSEEvent) error {
	data := strings.TrimSpace(string(ev.Data))
	if data == "[DONE]" {
		// Flush whatever the last chunks started: tool calls are complete
		// only once the stream says so (section 10).
		s.flushTools()
		s.stopped = true
		s.pending = append(s.pending, provider.Event{Kind: provider.Finished, Finish: s.finish})
		return nil
	}
	var chunk wireChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return fmt.Errorf("openai: decode chunk: %w", err)
	}
	if chunk.Error != nil {
		return &httpx.Error{Status: 0, Type: chunk.Error.Type, Message: chunk.Error.Message}
	}
	if chunk.Usage != nil {
		s.usage = provider.Usage{
			InputTokens:     chunk.Usage.PromptTokens,
			OutputTokens:    chunk.Usage.CompletionTokens,
			CachedTokens:    chunk.Usage.PromptTokensDetails.CachedTokens,
			ReasoningTokens: chunk.Usage.CompletionTokensDetails.ReasoningTokens,
		}
		u := s.usage
		s.pending = append(s.pending, provider.Event{Kind: provider.UsageReported, Usage: &u})
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			if !s.textOn {
				s.textOn = true
				s.pending = append(s.pending, provider.Event{Kind: provider.PartStarted, Index: 0})
			}
			s.text = append(s.text, choice.Delta.Content...)
			s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: 0, Delta: choice.Delta.Content})
		}
		for _, tc := range choice.Delta.ToolCalls {
			st := s.tools[tc.Index]
			if st == nil {
				st = &streamTool{}
				s.tools[tc.Index] = st
				s.toolList = append(s.toolList, tc.Index)
				s.pending = append(s.pending, provider.Event{Kind: provider.PartStarted, Index: tc.Index + 1})
			}
			if tc.ID != "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				st.args = append(st.args, tc.Function.Arguments...)
				s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: tc.Index + 1, Delta: tc.Function.Arguments})
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finish = decodeFinish(*choice.FinishReason)
			if s.textOn {
				s.pending = append(s.pending, provider.Event{Kind: provider.PartDone, Index: 0, Part: provider.TextPart{Text: string(s.text)}})
			}
			s.flushTools()
		}
	}
	return nil
}

// flushTools emits PartDone for every started tool call, in first-seen
// order — Chat Completions signals tool completion only through
// finish_reason, so arguments are complete at exactly this point.
func (s *stream) flushTools() {
	for _, idx := range s.toolList {
		st := s.tools[idx]
		if st == nil {
			continue
		}
		args := st.args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		s.pending = append(s.pending, provider.Event{
			Kind:  provider.PartDone,
			Index: idx + 1,
			Part:  provider.ToolCallPart{CallID: st.id, Name: st.name, Arguments: args},
		})
	}
	s.toolList = nil
	s.tools = map[int]*streamTool{}
}

// UploadFile implements provider.Provider with the Files API: a multipart
// POST returning the uploaded file's id.
func (c *Client) UploadFile(ctx context.Context, src provider.Source, name string) (provider.RemoteFile, error) {
	key, err := c.creds.Credential(ctx, c.Name())
	if err != nil {
		return provider.RemoteFile{}, fmt.Errorf("openai: credential: %w", err)
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
	_ = w.WriteField("purpose", "vision")
	if err := w.Close(); err != nil {
		return provider.RemoteFile{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/files", &buf)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+key)

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
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &up); err != nil {
		return provider.RemoteFile{}, fmt.Errorf("openai: decode upload: %w", err)
	}
	return provider.RemoteFile{ID: up.ID}, nil
}
