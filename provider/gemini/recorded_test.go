package gemini

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
		Model:           "gemini-2.5-pro",
		System:          []provider.Part{provider.TextPart{Text: "Be concise."}},
		Messages:        []provider.Message{{Role: provider.UserRole, Parts: []provider.Part{provider.TextPart{Text: "hello"}}}},
		MaxOutputTokens: 1024,
	}
}

func TestRecordedGenerateBasic(t *testing.T) {
	rec := recordtest.Load(t, "gemini", "generate-basic")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL+"/v1beta"))

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
	rec := recordtest.Load(t, "gemini", "generate-tools")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL+"/v1beta"))

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
	if len(calls) != 1 || len(rec.Expect.ToolCalls) != 1 {
		t.Fatalf("got %d calls, want %d", len(calls), len(rec.Expect.ToolCalls))
	}
	want := rec.Expect.ToolCalls[0]
	if calls[0].CallID != want.ID || calls[0].Name != want.Name {
		t.Errorf("call = %+v, want %s/%s", calls[0], want.ID, want.Name)
	}
	var gotArgs, wantArgs any
	if err := json.Unmarshal(calls[0].Arguments, &gotArgs); err != nil {
		t.Fatalf("args: %v", err)
	}
	if err := json.Unmarshal(want.Arguments, &wantArgs); err != nil {
		t.Fatalf("want args: %v", err)
	}
	gotRaw, _ := json.Marshal(gotArgs)
	wantRaw, _ := json.Marshal(wantArgs)
	if string(gotRaw) != string(wantRaw) {
		t.Errorf("args = %s, want %s", gotRaw, wantRaw)
	}
}

func TestRecordedGenerateError(t *testing.T) {
	rec := recordtest.Load(t, "gemini", "error-400")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL+"/v1beta"))

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
	rec := recordtest.Load(t, "gemini", "stream-basic")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL+"/v1beta"))

	s, err := c.Stream(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	recordtest.AssertSteps(t, rec.Expect.Stream, recordtest.Collect(t, s))
}

func TestRecordedStreamToolCall(t *testing.T) {
	rec := recordtest.Load(t, "gemini", "stream-tools")
	srv := recordtest.Serve(t, rec)
	c := New(recordtest.FixedCreds("test-key"), WithBaseURL(srv.URL+"/v1beta"))

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
