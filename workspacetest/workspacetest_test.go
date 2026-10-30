package workspacetest

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
)

func TestFakeClockAdvances(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
	c.Advance(90 * time.Second)
	if got := c.Now(); !got.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("after Advance(90s) Now() = %v, want %v", got, start.Add(90*time.Second))
	}
}

func TestSeqIDsAreDeterministic(t *testing.T) {
	a, b := NewSeqIDs("turn"), NewSeqIDs("turn")
	for i := 1; i <= 3; i++ {
		if x, y := a.NewID(), b.NewID(); x != y {
			t.Fatalf("generators diverged at %d: %s != %s", i, x, y)
		}
	}
	if got := a.NewID(); got != "turn-4" {
		t.Fatalf("NewID() = %q, want %q", got, "turn-4")
	}
}

func TestEventLogWaitForRecordsAndWakes(t *testing.T) {
	l := NewEventLog()
	l.Record(kernel.EventEnvelope{Name: "turn.appended", Owner: "me"})

	got, err := l.WaitFor(context.Background(), "turn.appended")
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if got.Name != "turn.appended" || got.Owner != "me" {
		t.Fatalf("WaitFor = %+v, want turn.appended/me", got)
	}
	if len(l.All()) != 1 {
		t.Fatalf("All() = %d events, want 1", len(l.All()))
	}
}

func TestEventLogWaitForBlocksUntilRecorded(t *testing.T) {
	l := NewEventLog()
	done := make(chan kernel.EventEnvelope, 1)
	go func() {
		env, _ := l.WaitFor(context.Background(), "conversation.created")
		done <- env
	}()

	select {
	case <-done:
		t.Fatal("WaitFor returned before the event was recorded")
	default:
	}

	l.Record(kernel.EventEnvelope{Name: "conversation.created"})
	env := <-done
	if env.Name != "conversation.created" {
		t.Fatalf("got %+v, want conversation.created", env)
	}
}

func TestEventLogWaitHonorsContext(t *testing.T) {
	l := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := l.WaitFor(ctx, "never"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitFor err = %v, want DeadlineExceeded", err)
	}
}

func TestScriptedProviderReplaysInOrder(t *testing.T) {
	p := NewScriptedProvider("scripted").ReplyText("hi").ReplyText("again")

	first, err := p.Generate(context.Background(), provider.Request{Model: "scripted"})
	if err != nil {
		t.Fatalf("Generate #1: %v", err)
	}
	if text := firstText(t, first); text != "hi" {
		t.Fatalf("first reply = %q, want %q", text, "hi")
	}

	second, err := p.Generate(context.Background(), provider.Request{Model: "scripted"})
	if err != nil {
		t.Fatalf("Generate #2: %v", err)
	}
	if text := firstText(t, second); text != "again" {
		t.Fatalf("second reply = %q, want %q", text, "again")
	}

	if _, err := p.Generate(context.Background(), provider.Request{}); !errors.Is(err, errScriptExhausted) {
		t.Fatalf("exhausted Generate err = %v, want errScriptExhausted", err)
	}
	if reqs := p.Requests(); len(reqs) != 3 {
		t.Fatalf("Requests() = %d, want 3", len(reqs))
	}
}

func TestScriptedStreamEmitsPartsThenEOF(t *testing.T) {
	p := NewScriptedProvider("scripted").ReplyToolCalls(
		provider.ToolCallPart{CallID: "c1", Name: "search", Arguments: []byte(`{}`)},
	)
	s, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	ev, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv #1: %v", err)
	}
	if ev.Kind != provider.PartDone {
		t.Fatalf("event kind = %v, want PartDone", ev.Kind)
	}
	if _, ok := ev.Part.(provider.ToolCallPart); !ok {
		t.Fatalf("part type = %T, want ToolCallPart", ev.Part)
	}

	ev, err = s.Recv()
	if err != nil {
		t.Fatalf("Recv #2: %v", err)
	}
	if ev.Kind != provider.Finished || ev.Finish != provider.FinishToolCalls {
		t.Fatalf("event = %+v, want Finished/tool_calls", ev)
	}

	if _, err = s.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv #3 err = %v, want io.EOF", err)
	}
}

// firstText pulls the text out of a reply's first part, failing the test if
// the scripted provider ever emits something else.
func firstText(t *testing.T, resp provider.Response) string {
	t.Helper()
	if len(resp.Message.Parts) == 0 {
		t.Fatal("reply has no parts")
	}
	tp, ok := resp.Message.Parts[0].(provider.TextPart)
	if !ok {
		t.Fatalf("first part is %T, want TextPart", resp.Message.Parts[0])
	}
	return tp.Text
}
