// Package recordtest replays recorded-HTTP transcripts against an
// adapter: it asserts the outgoing request matches the transcript
// (method, path, headers, JSON body) and serves the recorded response.
// Transcripts are authored from the vendors' public API documentation and
// stripped of secrets; `examples/record` refreshes live ones when credentials
// are available (section 16).
package recordtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asaidimu/workspace/provider"
)

// Recording is one authored exchange.
type Recording struct {
	Name    string `json:"name"`
	Request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status   int               `json:"status"`
		Headers  map[string]string `json:"headers"`
		Body     json.RawMessage   `json:"body"`
		BodyText string            `json:"body_text"`
	} `json:"response"`
	Expect struct {
		Finish       string `json:"finish"`
		Text         string `json:"text"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		RequestID    string `json:"request_id"`
		ToolCalls    []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
		Stream      []StreamStep `json:"stream"`
		ErrorStatus int          `json:"error_status"`
		ErrorType   string       `json:"error_type"`
	} `json:"expect"`
}

// StreamStep is one expected streaming event, compared in order.
type StreamStep struct {
	Kind     string `json:"kind"` // usage | part_started | part_delta | part_done | finished
	Index    int    `json:"index"`
	Delta    string `json:"delta"`
	Text     string `json:"text"`
	Finish   string `json:"finish"`
	Input    int    `json:"input_tokens"`
	Output   int    `json:"output_tokens"`
	ToolID   string `json:"tool_id"`
	ToolName string `json:"tool_name"`
	Args     string `json:"arguments"`
}

// Load reads testdata/http/<vendor>/<name>.json, relative to the calling
// adapter package's directory (every vendor package sits two levels below
// the module root).
func Load(t *testing.T, vendor, name string) Recording {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "http", vendor, name+".json"))
	if err != nil {
		t.Fatalf("read recording %s/%s: %v", vendor, name, err)
	}
	var rec Recording
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("parse recording %s/%s: %v", vendor, name, err)
	}
	return rec
}

// Serve starts a server that asserts the request and replays the response.
func Serve(t *testing.T, rec Recording) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != rec.Request.Method {
			t.Errorf("method = %s, want %s", r.Method, rec.Request.Method)
		}
		if r.URL.Path != rec.Request.Path {
			t.Errorf("path = %s, want %s", r.URL.Path, rec.Request.Path)
		}
		for k, v := range rec.Request.Headers {
			if got := r.Header.Get(k); got != v {
				t.Errorf("header %s = %q, want %q", k, got, v)
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var got, want any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body is not JSON: %v (%s)", err, body)
		}
		if err := json.Unmarshal(rec.Request.Body, &want); err != nil {
			t.Fatalf("recording body is not JSON: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			g, _ := json.MarshalIndent(got, "", "  ")
			wt, _ := json.MarshalIndent(want, "", "  ")
			t.Errorf("request body mismatch\ngot:\n%s\nwant:\n%s", g, wt)
		}

		for k, v := range rec.Response.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(rec.Response.Status)
		if rec.Response.BodyText != "" {
			io.WriteString(w, rec.Response.BodyText)
			return
		}
		w.Write(rec.Response.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// FixedCreds is a CredentialSource that always returns key.
type FixedCreds string

// Credential implements provider.CredentialSource.
func (f FixedCreds) Credential(ctx context.Context, name string) (string, error) {
	return string(f), nil
}

// FlattenText joins a message's text parts.
func FlattenText(m provider.Message) string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if tp, ok := p.(provider.TextPart); ok {
			sb.WriteString(tp.Text)
		}
	}
	return sb.String()
}

// Collect drains a stream until io.EOF.
func Collect(t *testing.T, s provider.Stream) []provider.Event {
	t.Helper()
	var events []provider.Event
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		events = append(events, ev)
	}
}

// AssertSteps compares drained events against the recorded steps.
func AssertSteps(t *testing.T, steps []StreamStep, events []provider.Event) {
	t.Helper()
	if len(events) != len(steps) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(steps), events)
	}
	kinds := map[provider.EventKind]string{
		provider.PartStarted:   "part_started",
		provider.PartDelta:     "part_delta",
		provider.PartDone:      "part_done",
		provider.UsageReported: "usage",
		provider.Finished:      "finished",
	}
	for i, want := range steps {
		got := events[i]
		kindName := kinds[got.Kind]
		if kindName != want.Kind {
			t.Errorf("event %d kind = %s, want %s", i, kindName, want.Kind)
			continue
		}
		switch want.Kind {
		case "part_delta":
			if got.Index != want.Index || got.Delta != want.Delta {
				t.Errorf("event %d = index %d delta %q, want %d %q", i, got.Index, got.Delta, want.Index, want.Delta)
			}
		case "part_started", "part_done":
			if got.Index != want.Index {
				t.Errorf("event %d index = %d, want %d", i, got.Index, want.Index)
			}
			if want.Kind == "part_done" && want.Text != "" {
				tp, ok := got.Part.(provider.TextPart)
				if !ok || tp.Text != want.Text {
					t.Errorf("event %d part = %+v, want text %q", i, got.Part, want.Text)
				}
			}
			if want.Kind == "part_done" && want.ToolID != "" {
				cp, ok := got.Part.(provider.ToolCallPart)
				if !ok || cp.CallID != want.ToolID || cp.Name != want.ToolName || string(cp.Arguments) != want.Args {
					t.Errorf("event %d part = %+v, want tool %s/%s %s", i, got.Part, want.ToolID, want.ToolName, want.Args)
				}
			}
		case "usage":
			if want.Input > 0 && got.Usage.InputTokens != want.Input {
				t.Errorf("event %d input tokens = %d, want %d", i, got.Usage.InputTokens, want.Input)
			}
			if want.Output > 0 && got.Usage.OutputTokens != want.Output {
				t.Errorf("event %d output tokens = %d, want %d", i, got.Usage.OutputTokens, want.Output)
			}
		case "finished":
			if string(got.Finish) != want.Finish {
				t.Errorf("event %d finish = %v, want %v", i, got.Finish, want.Finish)
			}
		}
	}
}
