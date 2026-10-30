package workspacetest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asaidimu/workspace/provider"
)

// ReplayMatch decides which requests a transcript serves. Every non-empty
// field must match; an empty field matches anything.
type ReplayMatch struct {
	// Provider must equal the replay provider's Name(). Transcripts are
	// recorded against a real vendor, so a test names its replay provider
	// after that vendor when it wants the match to be exact.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Contains must appear in the flattened request text (system plus every
	// text part), which is how a transcript targets one turn of a
	// tool-calling conversation without matching the others.
	Contains string `json:"contains,omitempty"`
}

// ReplayPart is the JSON form of a provider part in a transcript.
type ReplayPart struct {
	Type      string          `json:"type"` // text, thinking, tool_call, tool_result, image, file
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	RemoteID  string          `json:"remote_id,omitempty"`
}

func (w ReplayPart) part() (provider.Part, error) {
	switch w.Type {
	case "text":
		return provider.TextPart{Text: w.Text}, nil
	case "thinking":
		return provider.ThinkingPart{Text: w.Thinking, Signature: w.Signature}, nil
	case "tool_call":
		args := w.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		return provider.ToolCallPart{CallID: w.CallID, Name: w.Name, Arguments: args}, nil
	case "tool_result":
		out := w.Output
		if len(out) == 0 {
			out = json.RawMessage(`{}`)
		}
		return provider.ToolResultPart{CallID: w.CallID, Output: out, IsError: w.IsError}, nil
	case "image":
		return provider.ImagePart{RemoteID: w.RemoteID}, nil
	case "file":
		return provider.FilePart{Name: w.Name, RemoteID: w.RemoteID}, nil
	default:
		return nil, fmt.Errorf("workspacetest: replay part has unknown type %q", w.Type)
	}
}

// ReplayUsage is the JSON form of provider usage.
type ReplayUsage struct {
	InputTokens     int  `json:"input_tokens,omitempty"`
	OutputTokens    int  `json:"output_tokens,omitempty"`
	CachedTokens    int  `json:"cached_tokens,omitempty"`
	ReasoningTokens int  `json:"reasoning_tokens,omitempty"`
	Estimated       bool `json:"estimated,omitempty"`
}

func (w ReplayUsage) usage() provider.Usage {
	return provider.Usage{
		InputTokens:     w.InputTokens,
		OutputTokens:    w.OutputTokens,
		CachedTokens:    w.CachedTokens,
		ReasoningTokens: w.ReasoningTokens,
		Estimated:       w.Estimated,
	}
}

// ReplayEvent is one streamed event. DelayMS paces the event: Recv waits
// that long before returning it, honouring cancellation, which is how the
// streaming and cancel tests get a slow turn without a network.
type ReplayEvent struct {
	Kind    string       `json:"kind"` // part_started, part_delta, part_done, usage, finished
	Index   int          `json:"index,omitempty"`
	Delta   string       `json:"delta,omitempty"`
	Part    *ReplayPart  `json:"part,omitempty"`
	Usage   *ReplayUsage `json:"usage,omitempty"`
	Finish  string       `json:"finish,omitempty"`
	DelayMS int          `json:"delay_ms,omitempty"`
}

// ReplayResponse is the non-streaming answer. When it is absent the
// response is derived from the events: every part_done part, the last
// usage, and the finished reason.
type ReplayResponse struct {
	Parts     []ReplayPart `json:"parts"`
	Finish    string       `json:"finish,omitempty"`
	Usage     ReplayUsage  `json:"usage,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
}

// ReplayTranscript is one recorded exchange served by a ReplayProvider.
type ReplayTranscript struct {
	Name  string      `json:"name"`
	Match ReplayMatch `json:"match"`
	// Repeat keeps the transcript eligible after it has served a request,
	// so one file answers every matching turn of a conversation.
	Repeat bool `json:"repeat,omitempty"`
	// DelayMS paces Generate: the request takes at least this long, which
	// is what the run-cancel test blocks on.
	DelayMS  int             `json:"delay_ms,omitempty"`
	Events   []ReplayEvent   `json:"events,omitempty"`
	Response *ReplayResponse `json:"response,omitempty"`
}

// LoadReplayTranscripts reads every *.json file in dir, sorted by name so
// transcript order is deterministic across machines.
func LoadReplayTranscripts(t testing.TB, dir string) []ReplayTranscript {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read replay dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]ReplayTranscript, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read replay %s: %v", name, err)
		}
		var tr ReplayTranscript
		if err := json.Unmarshal(raw, &tr); err != nil {
			t.Fatalf("parse replay %s: %v", name, err)
		}
		if tr.Name == "" {
			tr.Name = strings.TrimSuffix(name, ".json")
		}
		out = append(out, tr)
	}
	return out
}

// replayState is one transcript plus whether it has already served a turn.
type replayState struct {
	tr   ReplayTranscript
	used bool
}

// ReplayProvider serves recorded transcripts instead of a network. It
// implements provider.Provider: Generate returns the transcript's
// response, Stream paces its events, cancellation aborts either call
// promptly, and every request is recorded for assertions.
type ReplayProvider struct {
	mu          sync.Mutex
	name        string
	transcripts []*replayState
	requests    []provider.Request
	open        int // in-flight Generate or Stream calls
}

// compile-time assertion that the double still satisfies the port.
var _ provider.Provider = (*ReplayProvider)(nil)

// NewReplayProvider returns a provider named name serving the given
// transcripts in order; the first matching, unused transcript answers each
// request.
func NewReplayProvider(name string, transcripts ...ReplayTranscript) *ReplayProvider {
	p := &ReplayProvider{name: name}
	for _, tr := range transcripts {
		p.transcripts = append(p.transcripts, &replayState{tr: tr})
	}
	return p
}

// Name implements provider.Provider.
func (p *ReplayProvider) Name() string { return p.name }

// Requests returns every Generate and Stream request, in order.
func (p *ReplayProvider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.Request, len(p.requests))
	copy(out, p.requests)
	return out
}

// Open reports how many provider calls are in flight right now. A cancel
// test waits for it to drop back to zero, which proves the provider
// request was closed rather than merely abandoned.
func (p *ReplayProvider) Open() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.open
}

func (p *ReplayProvider) begin(req provider.Request) *ReplayTranscript {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	p.open++
	flat := flattenRequest(req)
	for _, st := range p.transcripts {
		if st.used {
			continue
		}
		if !matches(st.tr.Match, p.name, req, flat) {
			continue
		}
		if !st.tr.Repeat {
			st.used = true
		}
		tr := st.tr
		return &tr
	}
	return nil
}

func (p *ReplayProvider) done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.open--
}

func matches(m ReplayMatch, providerName string, req provider.Request, flat string) bool {
	if m.Provider != "" && m.Provider != providerName {
		return false
	}
	if m.Model != "" && m.Model != req.Model {
		return false
	}
	if m.Contains != "" && !strings.Contains(flat, m.Contains) {
		return false
	}
	return true
}

// flattenRequest is the text a Contains match searches: the system parts
// followed by every text part in order.
func flattenRequest(req provider.Request) string {
	var sb strings.Builder
	writeParts := func(parts []provider.Part) {
		for _, part := range parts {
			if t, ok := part.(provider.TextPart); ok {
				sb.WriteString(t.Text)
				sb.WriteByte('\n')
			}
		}
	}
	writeParts(req.System)
	for _, m := range req.Messages {
		writeParts(m.Parts)
	}
	return sb.String()
}

// Models implements provider.Provider with the models the transcripts
// name, so a test can discover what to put in Request.Model.
func (p *ReplayProvider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []provider.ModelInfo
	for _, st := range p.transcripts {
		id := st.tr.Match.Model
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, provider.ModelInfo{ID: id, Capabilities: replayCaps()})
	}
	return out, nil
}

// Capabilities implements provider.Provider: a replay model can do
// everything, because the transcripts decide what actually happens.
func (p *ReplayProvider) Capabilities(model string) (provider.Capabilities, error) {
	return replayCaps(), nil
}

func replayCaps() provider.Capabilities {
	return provider.Capabilities{
		Streaming: true, NativeTools: true, StructuredOutput: true,
		Vision: true, PDF: true, Thinking: true, FileUpload: true,
		PromptCaching:   true,
		ContextWindow:   200_000,
		MaxOutputTokens: 32_000,
	}
}

// Generate implements provider.Provider. It blocks for the transcript's
// DelayMS, aborting early when ctx is cancelled.
func (p *ReplayProvider) Generate(ctx context.Context, req provider.Request) (provider.Response, error) {
	tr := p.begin(req)
	if tr == nil {
		p.done()
		return provider.Response{}, fmt.Errorf("workspacetest: no replay transcript matches the request (provider %s, model %q)", p.name, req.Model)
	}
	defer p.done()
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	if tr.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(tr.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		case <-timer.C:
		}
	}
	return p.buildResponse(req, *tr)
}

func (p *ReplayProvider) buildResponse(req provider.Request, tr ReplayTranscript) (provider.Response, error) {
	if tr.Response != nil {
		parts, err := partsOf(tr.Response.Parts)
		if err != nil {
			return provider.Response{}, err
		}
		return provider.Response{
			Message:           provider.Message{Role: provider.AssistantRole, Parts: parts},
			Usage:             tr.Response.Usage.usage(),
			Finish:            finishOrStop(tr.Response.Finish),
			Model:             req.Model,
			ProviderRequestID: tr.Response.RequestID,
		}, nil
	}
	resp := provider.Response{Message: provider.Message{Role: provider.AssistantRole}, Model: req.Model}
	for _, ev := range tr.Events {
		switch ev.Kind {
		case "part_done":
			if ev.Part == nil {
				return provider.Response{}, fmt.Errorf("workspacetest: replay %q part_done has no part", tr.Name)
			}
			part, err := ev.Part.part()
			if err != nil {
				return provider.Response{}, err
			}
			resp.Message.Parts = append(resp.Message.Parts, part)
		case "usage":
			if ev.Usage != nil {
				resp.Usage = ev.Usage.usage()
			}
		case "finished":
			resp.Finish = finishOrStop(ev.Finish)
		}
	}
	if resp.Finish == "" {
		resp.Finish = provider.FinishStop
	}
	return resp, nil
}

func partsOf(ws []ReplayPart) ([]provider.Part, error) {
	out := make([]provider.Part, 0, len(ws))
	for _, w := range ws {
		part, err := w.part()
		if err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}

func finishOrStop(s string) provider.FinishReason {
	if s == "" {
		return provider.FinishStop
	}
	return provider.FinishReason(s)
}

// Stream implements provider.Provider, pacing the transcript's events.
// Recv honours ctx cancellation between events and returns io.EOF after
// the finished event, as the port requires.
func (p *ReplayProvider) Stream(ctx context.Context, req provider.Request) (provider.Stream, error) {
	tr := p.begin(req)
	if tr == nil {
		p.done()
		return nil, fmt.Errorf("workspacetest: no replay transcript matches the request (provider %s, model %q)", p.name, req.Model)
	}
	s := &replayStream{
		p:      p,
		ctx:    ctx,
		events: tr.Events,
		name:   tr.Name,
	}
	if len(s.events) == 0 {
		// Nothing to stream: synthesise the finish so the contract holds.
		s.events = []ReplayEvent{{Kind: "finished", Finish: "stop"}}
	}
	return s, nil
}

// UploadFile implements provider.Provider with an unsupported failure:
// a replay has no blob backend.
func (p *ReplayProvider) UploadFile(ctx context.Context, src provider.Source, name string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errReplayUnsupported
}

// replayStream replays one transcript's events. The provider's open count
// drops exactly once, whether the stream finishes, is closed, or is
// cancelled.
type replayStream struct {
	p      *ReplayProvider
	ctx    context.Context
	events []ReplayEvent
	name   string
	i      int
	once   sync.Once
}

func (s *replayStream) release() { s.once.Do(s.p.done) }

// Recv implements provider.Stream.
func (s *replayStream) Recv() (provider.Event, error) {
	if s.i >= len(s.events) {
		s.release()
		return provider.Event{}, io.EOF
	}
	ev := s.events[s.i]
	if ev.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(ev.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
			s.release()
			return provider.Event{}, fmt.Errorf("workspacetest: replay stream %q cancelled: %w", s.name, s.ctx.Err())
		case <-timer.C:
		}
	}
	select {
	case <-s.ctx.Done():
		s.release()
		return provider.Event{}, fmt.Errorf("workspacetest: replay stream %q cancelled: %w", s.name, s.ctx.Err())
	default:
	}
	s.i++
	out, err := ev.event()
	if err != nil {
		s.release()
		return provider.Event{}, fmt.Errorf("workspacetest: replay stream %q: %w", s.name, err)
	}
	return out, nil
}

// Close implements provider.Stream and is safe at any time.
func (s *replayStream) Close() error {
	s.release()
	return nil
}

func (w ReplayEvent) event() (provider.Event, error) {
	out := provider.Event{Index: w.Index, Delta: w.Delta, Finish: finishOrStop(w.Finish)}
	switch w.Kind {
	case "part_started":
		out.Kind = provider.PartStarted
	case "part_delta":
		out.Kind = provider.PartDelta
	case "part_done":
		out.Kind = provider.PartDone
		if w.Part == nil {
			return provider.Event{}, fmt.Errorf("event %d part_done has no part", w.DelayMS)
		}
		part, err := w.Part.part()
		if err != nil {
			return provider.Event{}, err
		}
		out.Part = part
	case "usage":
		out.Kind = provider.UsageReported
		if w.Usage == nil {
			return provider.Event{}, fmt.Errorf("event usage has no usage")
		}
		u := w.Usage.usage()
		out.Usage = &u
	case "finished":
		out.Kind = provider.Finished
	default:
		return provider.Event{}, fmt.Errorf("unknown event kind %q", w.Kind)
	}
	return out, nil
}

// errReplayUnsupported mirrors the scripted double's failure: a replay
// provider has no file backend either.
var errReplayUnsupported = scriptedError("workspacetest: not supported by the replay provider")
