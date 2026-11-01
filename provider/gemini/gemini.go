// Package gemini adapts the Google Gemini generateContent API to the
// provider port: plain net/http, the shared retry client and the shared
// SSE decoder, with no vendor SDK.
package gemini

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

// Client is the Gemini adapter.
type Client struct {
	base  string
	http  *httpx.Client
	creds provider.CredentialSource
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the adapter at another host (tests, proxies).
func WithBaseURL(u string) Option { return func(c *Client) { c.base = strings.TrimSuffix(u, "/") } }

// WithHTTP replaces the shared retry client.
func WithHTTP(h *httpx.Client) Option { return func(c *Client) { c.http = h } }

// New returns a Gemini adapter that reads its API key from creds on every
// call, so a rotated key needs no new client.
func New(creds provider.CredentialSource, opts ...Option) *Client {
	c := &Client{
		base:  "https://generativelanguage.googleapis.com/v1beta",
		http:  httpx.New(),
		creds: creds,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "gemini" }

// Models implements provider.Provider; pricing is list price in micro-USD
// per million tokens.
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
	return provider.Capabilities{}, fmt.Errorf("gemini: unknown model %q", model)
}


// --- request mapping ---

type wireContent struct {
	Role  string     `json:"role"`
	Parts []wirePart `json:"parts"`
}

type wirePart struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *wireData         `json:"inlineData,omitempty"`
	FileData         *wireFileData     `json:"fileData,omitempty"`
	FunctionCall     *wireFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResp `json:"functionResponse,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
}

type wireData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFileData struct {
	FileURI  string `json:"fileUri"`
	MimeType string `json:"mimeType,omitempty"`
}

type wireFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type wireFunctionResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

func (c *Client) buildBody(ctx context.Context, req provider.Request, stream bool) (map[string]any, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("gemini: request has no model")
	}
	caps, err := c.Capabilities(req.Model)
	if err != nil {
		return nil, err
	}
	if len(req.Tools) > 0 && !caps.NativeTools {
		return nil, fmt.Errorf("gemini: model %q cannot take tools", req.Model)
	}

	body := map[string]any{}
	gen := map[string]any{}
	maxOut := req.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = caps.MaxOutputTokens
	}
	if maxOut > 0 {
		gen["maxOutputTokens"] = maxOut
	}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		gen["stopSequences"] = req.Stop
	}
	if len(req.OutputSchema) > 0 {
		var schema any
		if err := json.Unmarshal(req.OutputSchema, &schema); err != nil {
			return nil, fmt.Errorf("gemini: decode output schema: %w", err)
		}
		gen["responseMimeType"] = "application/json"
		gen["responseSchema"] = geminiSchema(schema)
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}

	if s := systemText(req.System); s != "" {
		body["systemInstruction"] = wireContent{Parts: []wirePart{{Text: s}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			var params any = map[string]any{}
			if len(t.Schema) > 0 {
				if err := json.Unmarshal(t.Schema, &params); err != nil {
					return nil, fmt.Errorf("gemini: tool %q schema: %w", t.Name, err)
				}
			}
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  geminiSchema(params),
			})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}

	contents, err := wireContents(ctx, req, caps)
	if err != nil {
		return nil, err
	}
	body["contents"] = contents
	_ = stream // generateContent and streamGenerateContent share one body
	return body, nil
}

// geminiSchema rewrites a JSON Schema into the shape Gemini's Schema proto
// accepts. The one difference that bites in practice is nullable types: the
// workspace's converter emits `"type": ["string","null"]`, while Gemini wants
// a single enum value and rejects the array with INVALID_ARGUMENT. Dropping
// "null" keeps the constraint that matters (the value is a string) and the
// field stays optional through `required`.
func geminiSchema(node any) any {
	m, ok := node.(map[string]any)
	if !ok {
		return node
	}
	if t, ok := m["type"]; ok {
		m["type"] = geminiType(t)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, prop := range props {
			props[name] = geminiSchema(prop)
		}
	}
	if items, ok := m["items"]; ok {
		m["items"] = geminiSchema(items)
	}
	return m
}

func geminiType(t any) any {
	list, ok := t.([]any)
	if !ok {
		return t
	}
	for _, v := range list {
		if name, ok := v.(string); ok && name != "null" {
			return name
		}
	}
	return t
}

// wireContents maps each message; tool results come back as a user
// content holding functionResponse parts, assistant turns as model.
func wireContents(ctx context.Context, req provider.Request, caps provider.Capabilities) ([]wireContent, error) {
	var out []wireContent
	for _, m := range req.Messages {
		wc := wireContent{Role: "user", Parts: []wirePart{}}
		if m.Role == provider.AssistantRole {
			wc.Role = "model"
		}
		for _, p := range m.Parts {
			switch v := p.(type) {
			case provider.TextPart:
				wc.Parts = append(wc.Parts, wirePart{Text: v.Text})
			case provider.ThinkingPart:
				// Thoughts are not sent back (section 6's table).
				continue
			case provider.ImagePart:
				if !caps.Vision {
					return nil, fmt.Errorf("gemini: model cannot take images")
				}
				part, err := imagePart(ctx, v.Source, v.RemoteID)
				if err != nil {
					return nil, err
				}
				wc.Parts = append(wc.Parts, part)
			case provider.FilePart:
				if v.RemoteID != "" {
					wc.Parts = append(wc.Parts, wirePart{FileData: &wireFileData{FileURI: v.RemoteID}})
				} else if v.Source != nil {
					data, err := readAll(ctx, v.Source)
					if err != nil {
						return nil, err
					}
					wc.Parts = append(wc.Parts, wirePart{InlineData: &wireData{MimeType: v.Source.MediaType(), Data: data}})
				} else {
					return nil, fmt.Errorf("gemini: file part has neither a source nor an uploaded id")
				}
			case provider.ToolCallPart:
				args := v.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				wc.Parts = append(wc.Parts, wirePart{FunctionCall: &wireFunctionCall{Name: v.Name, Args: args}})
			case provider.ToolResultPart:
				resp := v.Output
				if !json.Valid(resp) {
					// The tool's output may be free text; wrap it so the
					// vendor's JSON envelope stays well formed.
					wrapped, _ := json.Marshal(map[string]string{"result": string(resp)})
					resp = wrapped
				}
				wc.Parts = append(wc.Parts, wirePart{FunctionResponse: &wireFunctionResp{Name: v.CallID, Response: resp}})
				wc.Role = "user"
			default:
				return nil, fmt.Errorf("gemini: unsupported part %T", p)
			}
		}
		if len(wc.Parts) > 0 {
			out = append(out, wc)
		}
	}
	return out, nil
}

func imagePart(ctx context.Context, src provider.Source, remoteID string) (wirePart, error) {
	switch {
	case remoteID != "":
		return wirePart{FileData: &wireFileData{FileURI: remoteID}}, nil
	case src != nil:
		data, err := readAll(ctx, src)
		if err != nil {
			return wirePart{}, err
		}
		return wirePart{InlineData: &wireData{MimeType: src.MediaType(), Data: data}}, nil
	default:
		return wirePart{}, fmt.Errorf("gemini: image part has neither a source nor an uploaded id")
	}
}

func readAll(ctx context.Context, src provider.Source) (string, error) {
	rc, err := src.Open(ctx)
	if err != nil {
		return "", fmt.Errorf("gemini: open source: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("gemini: read source: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
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

// --- response mapping ---

type wireResponse struct {
	Candidates []struct {
		Content struct {
			Parts []wirePart `json:"parts"`
			Role  string     `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
	ResponseID   string `json:"responseId"`
}

func (c *Client) headers(ctx context.Context) (map[string]string, error) {
	key, err := c.creds.Credential(ctx, c.Name())
	if err != nil {
		return nil, fmt.Errorf("gemini: credential: %w", err)
	}
	if key == "" {
		return nil, fmt.Errorf("gemini: empty API key")
	}
	return map[string]string{"x-goog-api-key": key}, nil
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
	url := fmt.Sprintf("%s/models/%s:generateContent", c.base, req.Model)
	res, err := c.http.JSON(ctx, url, hdr, body)
	if err != nil {
		return provider.Response{}, err
	}
	if res.Status != http.StatusOK {
		return provider.Response{}, mapAPIError(res.Status, res.Body)
	}
	var wr wireResponse
	if err := json.Unmarshal(res.Body, &wr); err != nil {
		return provider.Response{}, fmt.Errorf("gemini: decode response: %w", err)
	}
	return decodeResponse(wr, req.Model)
}

func decodeResponse(wr wireResponse, model string) (provider.Response, error) {
	if len(wr.Candidates) == 0 {
		return provider.Response{}, fmt.Errorf("gemini: response has no candidates")
	}
	cand := wr.Candidates[0]
	parts, err := decodeParts(cand.Content.Parts)
	if err != nil {
		return provider.Response{}, err
	}
	finish := decodeFinish(cand.FinishReason, parts)
	return provider.Response{
		Message: provider.Message{Role: provider.AssistantRole, Parts: parts},
		Usage: provider.Usage{
			InputTokens:     wr.UsageMetadata.PromptTokenCount,
			OutputTokens:    wr.UsageMetadata.CandidatesTokenCount,
			ReasoningTokens: wr.UsageMetadata.ThoughtsTokenCount,
			CachedTokens:    wr.UsageMetadata.CachedContentTokenCount,
		},
		Finish:            finish,
		Model:             orModel(wr.ModelVersion, model),
		ProviderRequestID: wr.ResponseID,
	}, nil
}

func decodeParts(wps []wirePart) ([]provider.Part, error) {
	parts := make([]provider.Part, 0, len(wps))
	for _, p := range wps {
		switch {
		case p.Thought:
			parts = append(parts, provider.ThinkingPart{Text: p.Text})
		case p.Text != "":
			parts = append(parts, provider.TextPart{Text: p.Text})
		case p.FunctionCall != nil:
			args := p.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			// Gemini has no call ids: the function name is the id.
			parts = append(parts, provider.ToolCallPart{CallID: p.FunctionCall.Name, Name: p.FunctionCall.Name, Arguments: args})
		case p.InlineData != nil || p.FileData != nil:
			// Model-produced media is out of scope for the port.
			return nil, fmt.Errorf("gemini: media in response is not supported")
		default:
			return nil, fmt.Errorf("gemini: unrecognized response part")
		}
	}
	return parts, nil
}

func decodeFinish(stop string, parts []provider.Part) provider.FinishReason {
	switch stop {
	case "MAX_TOKENS":
		return provider.FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return provider.FinishContentFilter
	case "OTHER":
		return provider.FinishError
	default: // STOP
		for _, p := range parts {
			if _, ok := p.(provider.ToolCallPart); ok {
				return provider.FinishToolCalls
			}
		}
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
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func mapAPIError(status int, body []byte) error {
	var we wireError
	_ = json.Unmarshal(body, &we)
	msgType := we.Error.Status
	if msgType == "" {
		msgType = http.StatusText(status)
	}
	code := we.Error.Code
	if code == 0 {
		code = status
	}
	return &httpx.Error{Status: code, Type: msgType, Message: we.Error.Message}
}

// --- streaming ---

// Stream implements provider.Provider. Gemini streams over plain SSE
// (`alt=sse`) with no terminator event: the last chunk carries
// finishReason and the body then closes.
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
		return nil, fmt.Errorf("gemini: model %q cannot stream", req.Model)
	}
	body, err := c.buildBody(ctx, req, true)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", c.base, req.Model)
	conn, err := c.http.Stream(ctx, url, hdr, body)
	if err != nil {
		if se, ok := err.(*httpx.StreamError); ok {
			return nil, mapAPIError(se.Status, se.Body)
		}
		return nil, err
	}
	return &stream{
		dec:     provider.NewSSEDecoder(conn.Body),
		body:    conn.Body,
		partIdx: -1,
	}, nil
}

type stream struct {
	dec     *provider.SSEDecoder
	body    io.ReadCloser
	pending []provider.Event
	text    []byte
	textOn  bool
	partSeq int  // indexes handed out so far
	partIdx int  // currently open text part, -1 when none
	kind    byte // 't' thought / 'x' text of the open part
	sawTool bool
	usage   provider.Usage
	finish  provider.FinishReason
	stopped bool
	closed  bool
}

func (s *stream) openPart(kind byte) {
	if s.textOn && s.kind == kind {
		return
	}
	s.flushText()
	s.partIdx = s.partSeq
	s.partSeq++
	s.kind = kind
	s.textOn = true
	s.pending = append(s.pending, provider.Event{Kind: provider.PartStarted, Index: s.partIdx})
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
			// The vendor's last chunk already carried finishReason; emit
			// Finished before the contract's io.EOF (section 10).
			if !s.closed {
				s.flushText()
				s.pending = append(s.pending, provider.Event{Kind: provider.Finished, Finish: s.finishOrDefault()})
				s.closed = true
			} else {
				s.stopped = true
			}
			continue
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
	Candidates []struct {
		Content struct {
			Parts []wirePart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (s *stream) mapFrame(ev provider.SSEEvent) error {
	if len(ev.Data) == 0 {
		return nil
	}
	var chunk wireChunk
	if err := json.Unmarshal(ev.Data, &chunk); err != nil {
		return fmt.Errorf("gemini: decode chunk: %w", err)
	}
	if chunk.Error != nil {
		code := chunk.Error.Code
		if code == 0 {
			code = 500
		}
		return &httpx.Error{Status: code, Type: chunk.Error.Status, Message: chunk.Error.Message}
	}
	if chunk.UsageMetadata != nil {
		u := provider.Usage{
			InputTokens:     chunk.UsageMetadata.PromptTokenCount,
			OutputTokens:    chunk.UsageMetadata.CandidatesTokenCount,
			ReasoningTokens: chunk.UsageMetadata.ThoughtsTokenCount,
			CachedTokens:    chunk.UsageMetadata.CachedContentTokenCount,
		}
		s.usage = u
	}
	for _, cand := range chunk.Candidates {
		for _, p := range cand.Content.Parts {
			switch {
			case p.Thought:
				// Display material; streamed but never executed.
				s.openPart('t')
				s.text = append(s.text, p.Text...)
				s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: s.partIdx, Delta: p.Text})
			case p.Text != "":
				s.openPart('x')
				s.text = append(s.text, p.Text...)
				s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: s.partIdx, Delta: p.Text})
			case p.FunctionCall != nil:
				// Gemini sends whole calls in one chunk, so one chunk is
				// start + arguments + done.
				args := p.FunctionCall.Args
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				s.flushText()
				idx := s.partSeq
				s.partSeq++
				s.pending = append(s.pending, provider.Event{Kind: provider.PartStarted, Index: idx})
				s.pending = append(s.pending, provider.Event{Kind: provider.PartDelta, Index: idx, Delta: string(args)})
				s.pending = append(s.pending, provider.Event{
					Kind:  provider.PartDone,
					Index: idx,
					Part:  provider.ToolCallPart{CallID: p.FunctionCall.Name, Name: p.FunctionCall.Name, Arguments: args},
				})
				s.sawTool = true
			}
		}
		if cand.FinishReason != "" {
			s.finish = decodeFinish(cand.FinishReason, nil)
			if s.sawTool && s.finish == provider.FinishStop {
				s.finish = provider.FinishToolCalls
			}
			s.flushText()
			u := s.usage
			if u.InputTokens > 0 || u.OutputTokens > 0 {
				s.pending = append(s.pending, provider.Event{Kind: provider.UsageReported, Usage: &u})
			}
			s.pending = append(s.pending, provider.Event{Kind: provider.Finished, Finish: s.finish})
			s.closed = true
		}
	}
	return nil
}

// flushText emits the closing PartDone for the open text part, once.
func (s *stream) flushText() {
	if !s.textOn {
		return
	}
	s.textOn = false
	idx := s.partIdx
	s.partIdx = -1
	s.pending = append(s.pending, provider.Event{Kind: provider.PartDone, Index: idx, Part: provider.TextPart{Text: string(s.text)}})
	s.text = s.text[:0]
}

func (s *stream) finishOrDefault() provider.FinishReason {
	if s.finish == "" {
		return provider.FinishStop
	}
	return s.finish
}

// UploadFile implements provider.Provider with the Files API: a
// multipart POST returning the uploaded file's resource name.
func (c *Client) UploadFile(ctx context.Context, src provider.Source, name string) (provider.RemoteFile, error) {
	key, err := c.creds.Credential(ctx, c.Name())
	if err != nil {
		return provider.RemoteFile{}, fmt.Errorf("gemini: credential: %w", err)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/files", &buf)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("x-goog-api-key", key)

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
		File struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		} `json:"file"`
	}
	if err := json.Unmarshal(body, &up); err != nil {
		return provider.RemoteFile{}, fmt.Errorf("gemini: decode upload: %w", err)
	}
	return provider.RemoteFile{ID: orName(up.File.URI, up.File.Name)}, nil
}

func orName(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
