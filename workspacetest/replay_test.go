package workspacetest

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/asaidimu/workspace/provider"
)

func flattenText(m provider.Message) string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if t, ok := p.(provider.TextPart); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

func replayRequest(contains string) provider.Request {
	msgs := []provider.Message{{
		Role:  provider.UserRole,
		Parts: []provider.Part{provider.TextPart{Text: "hello"}},
	}}
	if contains != "" {
		msgs[0].Parts = append(msgs[0].Parts, provider.TextPart{Text: contains})
	}
	return provider.Request{Model: "replay-model", Messages: msgs}
}

func loadAll(t *testing.T) []ReplayTranscript {
	t.Helper()
	return LoadReplayTranscripts(t, "../testdata/replays")
}

func byName(t *testing.T, name string) ReplayTranscript {
	t.Helper()
	for _, tr := range loadAll(t) {
		if tr.Name == name {
			return tr
		}
	}
	t.Fatalf("no replay transcript named %q", name)
	return ReplayTranscript{}
}

func TestReplayGenerateServesAMatchingTranscript(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "greeting"))

	resp, err := p.Generate(context.Background(), replayRequest(""))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := flattenText(resp.Message); got != "Hello from replay." {
		t.Errorf("text = %q", got)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 6 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.ProviderRequestID != "replay-req-1" {
		t.Errorf("request id = %q", resp.ProviderRequestID)
	}
	if n := len(p.Requests()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	if open := p.Open(); open != 0 {
		t.Errorf("open = %d after Generate, want 0", open)
	}

	// Repeat keeps serving; a non-repeat transcript is consumed once.
	if _, err := p.Generate(context.Background(), replayRequest("")); err != nil {
		t.Errorf("second Generate: %v", err)
	}
}

func TestReplayGenerateFailsWhenNothingMatches(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "greeting"))
	other := replayRequest("")
	other.Model = "other-model"
	if _, err := p.Generate(context.Background(), other); err == nil {
		t.Fatal("expected a no-match error")
	}
	if open := p.Open(); open != 0 {
		t.Errorf("open = %d after a failed match, want 0", open)
	}
}

func TestReplayMatchContainsTargetsOneTurn(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "tool-call"))

	resp, err := p.Generate(context.Background(), replayRequest("CALL_ME"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Finish != provider.FinishToolCalls {
		t.Errorf("finish = %v, want tool_calls", resp.Finish)
	}
	var call provider.ToolCallPart
	for _, part := range resp.Message.Parts {
		if c, ok := part.(provider.ToolCallPart); ok {
			call = c
		}
	}
	if call.Name != "lookup" || call.CallID != "call-1" {
		t.Errorf("call = %+v", call)
	}
	if !strings.Contains(string(call.Arguments), "CALL_ME") {
		t.Errorf("arguments = %s", call.Arguments)
	}
}

func TestReplayStreamEmitsOrderedEvents(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "streaming"))
	s, err := p.Stream(context.Background(), replayRequest("STREAM_ME"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	var kinds []provider.Event
	var text strings.Builder
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		kinds = append(kinds, ev)
		if ev.Kind == provider.PartDelta {
			text.WriteString(ev.Delta)
		}
	}

	want := []provider.EventKind{
		provider.PartStarted, provider.PartDelta, provider.PartDelta,
		provider.PartDone, provider.UsageReported, provider.Finished,
	}
	if len(kinds) != len(want) {
		t.Fatalf("got %d events (%+v), want %d", len(kinds), kinds, len(want))
	}
	for i, k := range want {
		if kinds[i].Kind != k {
			t.Errorf("event %d kind = %v, want %v", i, kinds[i].Kind, k)
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("deltas = %q, want %q", text.String(), "Hello world")
	}
	// Recv after Finished returns io.EOF (checked by loop exit), and the
	// open count is back to zero once the stream drained.
	if open := p.Open(); open != 0 {
		t.Errorf("open = %d after drain, want 0", open)
	}
}

// The gate's cancel target: cancelling a streaming turn closes the
// provider request within one second.
func TestReplayStreamCancelClosesTheRequestWithinOneSecond(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "streaming-slow"))
	ctx, cancel := context.WithCancel(context.Background())
	s, err := p.Stream(ctx, replayRequest("SLOW_STREAM"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	// First event is immediate; the second is paced 30 s out. Cancel while
	// it waits.
	if _, err := s.Recv(); err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if open := p.Open(); open != 1 {
		t.Fatalf("open = %d mid-stream, want 1", open)
	}

	start := time.Now()
	cancel()
	_, err = s.Recv()
	elapsed := time.Since(start)
	if err == nil || err == io.EOF {
		t.Fatalf("Recv after cancel = %v, want a cancellation error", err)
	}
	if elapsed > time.Second {
		t.Errorf("cancel took %v, want within 1s", elapsed)
	}
	// The provider request must be closed, not merely abandoned.
	waitFor(t, "the provider request to close", func() bool { return p.Open() == 0 }, time.Second)
}

// The same one-second target for a non-streaming call: cancelling a run
// whose Generate is in flight closes the provider request promptly.
func TestReplayGenerateCancelClosesTheRequestWithinOneSecond(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "slow-generate"))
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := p.Generate(ctx, replayRequest("SLOW_GENERATE"))
		errCh <- err
	}()
	waitFor(t, "the generate to be in flight", func() bool { return p.Open() == 1 }, time.Second)

	start := time.Now()
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Generate returned nil after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("Generate did not return within 1s of cancel")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancel took %v, want within 1s", elapsed)
	}
	waitFor(t, "the provider request to close", func() bool { return p.Open() == 0 }, time.Second)
}

func TestReplayStreamCloseIsSafeAtAnyTime(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "streaming"))
	s, err := p.Stream(context.Background(), replayRequest("STREAM_ME"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}
	waitFor(t, "the provider request to close", func() bool { return p.Open() == 0 }, time.Second)
}

func TestReplayModelsAndCapabilities(t *testing.T) {
	p := NewReplayProvider("replay", byName(t, "greeting"))
	models, err := p.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "replay-model" {
		t.Fatalf("Models = %+v, %v", models, err)
	}
	caps, err := p.Capabilities("anything")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Streaming || !caps.NativeTools {
		t.Errorf("caps = %+v", caps)
	}
	if _, err := p.UploadFile(context.Background(), nil, "x.png"); err == nil {
		t.Error("UploadFile should be unsupported")
	}
}

// waitFor spins on cond until it holds or the deadline passes; the
// deadline doubles as the assertion's tolerance.
func waitFor(t *testing.T, what string, cond func() bool, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
