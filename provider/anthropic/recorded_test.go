package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/internal/httpx"
	"github.com/asaidimu/workspace/provider/internal/recordtest"
)

func sampleRequest() provider.Request {
	return provider.Request{
		Model:           "claude-sonnet-4-5-20250929",
		System:          []provider.Part{provider.TextPart{Text: "Be concise."}},
		Messages:        []provider.Message{{Role: provider.UserRole, Parts: []provider.Part{provider.TextPart{Text: "hello"}}}},
		MaxOutputTokens: 1024,
	}
}

func TestRecordedGenerateBasic(t *testing.T) {
	rec := recordtest.Load(t, "anthropic", "generate-basic")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL))

	resp, err := c.Generate(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Finish != provider.FinishReason(rec.Expect.Finish) {
		t.Errorf("finish = %v, want %v", resp.Finish, rec.Expect.Finish)
	}
	if got := recordtest.FlattenText(resp.Message); got != rec.Expect.Text {
		t.Errorf("text = %q, want %q", got, rec.Expect.Text)
	}
	if resp.Usage.InputTokens != rec.Expect.InputTokens || resp.Usage.OutputTokens != rec.Expect.OutputTokens {
		t.Errorf("usage = %+v, want in/out %d/%d", resp.Usage, rec.Expect.InputTokens, rec.Expect.OutputTokens)
	}
	if resp.ProviderRequestID != rec.Expect.RequestID {
		t.Errorf("request id = %q, want %q", resp.ProviderRequestID, rec.Expect.RequestID)
	}
}

func TestRecordedGenerateToolCall(t *testing.T) {
	rec := recordtest.Load(t, "anthropic", "generate-tools")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL))

	req := sampleRequest()
	req.Tools = []provider.ToolSpec{{
		Name:        "getWeather",
		Description: "Get current weather for a city.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}
	resp, err := c.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Finish != provider.FinishToolCalls {
		t.Errorf("finish = %v, want tool_calls", resp.Finish)
	}
	var calls []provider.ToolCallPart
	for _, p := range resp.Message.Parts {
		if call, ok := p.(provider.ToolCallPart); ok {
			calls = append(calls, call)
		}
	}
	if len(calls) != len(rec.Expect.ToolCalls) {
		t.Fatalf("got %d tool calls, want %d", len(calls), len(rec.Expect.ToolCalls))
	}
	for i, want := range rec.Expect.ToolCalls {
		if calls[i].CallID != want.ID || calls[i].Name != want.Name {
			t.Errorf("call %d = %+v, want %s/%s", i, calls[i], want.ID, want.Name)
		}
		var gotArgs, wantArgs any
		_ = json.Unmarshal(calls[i].Arguments, &gotArgs)
		_ = json.Unmarshal(want.Arguments, &wantArgs)
		if gotArgs == nil || wantArgs == nil {
			t.Errorf("call %d args = %s, want %s", i, calls[i].Arguments, want.Arguments)
			continue
		}
		if string(mustJSON(t, gotArgs)) != string(mustJSON(t, wantArgs)) {
			t.Errorf("call %d args = %s, want %s", i, calls[i].Arguments, want.Arguments)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestRecordedGenerateError(t *testing.T) {
	rec := recordtest.Load(t, "anthropic", "error-400")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL))

	_, err := c.Generate(context.Background(), sampleRequest())
	var he *httpx.Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *httpx.Error", err)
	}
	if he.Status != rec.Expect.ErrorStatus || he.Type != rec.Expect.ErrorType {
		t.Fatalf("error = %+v, want %d/%s", he, rec.Expect.ErrorStatus, rec.Expect.ErrorType)
	}
}

func TestRecordedStreamBasic(t *testing.T) {
	rec := recordtest.Load(t, "anthropic", "stream-basic")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL))

	s, err := c.Stream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	recordtest.AssertSteps(t, rec.Expect.Stream, recordtest.Collect(t, s))
}

func TestRecordedStreamToolCall(t *testing.T) {
	rec := recordtest.Load(t, "anthropic", "stream-tools")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL))

	req := sampleRequest()
	req.Tools = []provider.ToolSpec{{Name: "getWeather", Description: "weather", Schema: json.RawMessage(`{"type":"object"}`)}}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	recordtest.AssertSteps(t, rec.Expect.Stream, recordtest.Collect(t, s))
}

func TestCatalogueCoherence(t *testing.T) {
	c := New(recordtest.FixedCreds("k"))
	models, err := c.Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("Models: %v, %d", err, len(models))
	}
	for _, m := range models {
		caps, err := c.Capabilities(m.ID)
		if err != nil {
			t.Fatalf("Capabilities(%s): %v", m.ID, err)
		}
		if !caps.Streaming || !caps.NativeTools || caps.ContextWindow <= 0 {
			t.Errorf("model %s has implausible capabilities: %+v", m.ID, caps)
		}
		if m.Pricing.InputMicrosPerMTok <= 0 || m.Pricing.OutputMicrosPerMTok <= 0 {
			t.Errorf("model %s has no pricing", m.ID)
		}
	}
	if _, err := c.Capabilities("nope"); err == nil {
		t.Error("unknown model was accepted")
	}
}
