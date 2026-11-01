package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/prompt"
	"github.com/asaidimu/workspace/provider"
)

// --- fakes ---

type fakeProvider struct {
	name  string
	caps  provider.Capabilities
	req   provider.Request
	reply string
	calls int
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Capabilities(string) (provider.Capabilities, error) { return f.caps, nil }

func (f *fakeProvider) Models(_ context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (f *fakeProvider) Stream(_ context.Context, _ provider.Request) (provider.Stream, error) {
	return nil, errors.New("streaming is not used by the compactor")
}

func (f *fakeProvider) UploadFile(_ context.Context, _ provider.Source, _ string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errors.New("uploads are not used by the compactor")
}

func (f *fakeProvider) Generate(_ context.Context, req provider.Request) (provider.Response, error) {
	f.calls++
	f.req = req
	return provider.Response{
		Message: provider.Message{
			Role:  provider.AssistantRole,
			Parts: []provider.Part{provider.TextPart{Text: f.reply}},
		},
		Finish: provider.FinishStop,
	}, nil
}

type fakeHistory struct {
	snap     prompt.Snapshot
	appended []domain.Turn
	keys     []string
}

func (f *fakeHistory) Read(_ context.Context, _ kernel.Principal, _ string) (prompt.Snapshot, error) {
	return f.snap, nil
}

func (f *fakeHistory) Append(_ context.Context, _ kernel.Principal, _ string, turn domain.Turn, key string) (domain.Turn, error) {
	f.appended = append(f.appended, turn)
	f.keys = append(f.keys, key)
	return turn, nil
}

func textTurn(id, actor, text string) domain.Turn {
	return domain.Turn{
		ID: id, Version: 1,
		Actor:  domain.Actor{Kind: domain.ActorKind(actor), ID: actor},
		Blocks: []domain.Block{{Type: "text", Value: map[string]any{"text": text}}},
	}
}

func toolRoundTurns(callID, output string) []domain.Turn {
	return []domain.Turn{
		{ID: "turn-" + callID, Version: 1, Actor: domain.Actor{Kind: domain.ActorAgent, ID: "a"},
			Blocks: []domain.Block{{Type: "tool_call", Value: map[string]any{
				"call_id": callID, "name": "shell", "arguments": json.RawMessage(`{"command":"go test"}`)}}}},
		{ID: "res-" + callID, Version: 1, Actor: domain.Actor{Kind: domain.ActorTool, ID: "shell"},
			Blocks: []domain.Block{{Type: "tool_result", Value: map[string]any{
				"call_id": callID, "output": output}}}},
	}
}

const digesterReply = `{
  "context_summary": "The user asked about Split and a fix was drafted.",
  "tool_digests": [
    {"call_id": "call_1", "text": "Ran the failing test: TestSplit fails on empty input"},
    {"call_id": "ghost_9", "text": "This call never existed"}
  ]
}`

// --- ModelCompactor ---

func TestModelCompactorProducesBothKinds(t *testing.T) {
	fp := &fakeProvider{name: "fake", reply: digesterReply}
	mc := &ModelCompactor{Provider: fp, Model: "small/fast"}

	omitted := []domain.Turn{
		textTurn("t1", "user", "why does TestSplit fail?"),
		textTurn("t2", "agent", "let me look"),
		textTurn("t3", "user", "and the fix?"),
		textTurn("t4", "agent", "a guard clause"),
	}
	var rounds []prompt.ToolRound
	for _, tt := range toolRoundTurns("call_1", strings.Repeat("FAIL", 200)) {
		rounds = append(rounds, prompt.ToolRound{CallID: "call_1", Name: "shell", Output: strings.Repeat("FAIL", 200), TurnRef: tt.Ref()})
	}
	rounds = append(rounds, prompt.ToolRound{CallID: "call_2", Name: "read_file", Output: "ok", TurnRef: domain.TurnRef{ID: "res-call_2", Version: 1}})

	out, err := mc.Compact(context.Background(), prompt.CompactionInput{
		Snapshot:   prompt.Snapshot{Conversation: domain.Conversation{ID: "c1"}},
		Omitted:    omitted,
		ToolRounds: rounds,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if fp.calls != 1 {
		t.Fatalf("compaction must be one model call, got %d", fp.calls)
	}

	var ctxSummary, toolSummary bool
	for _, s := range out {
		switch s.Kind {
		case prompt.KindContext:
			ctxSummary = true
			if s.Context.Text == "" || len(s.Context.Covers) != 4 {
				t.Fatalf("context summary must carry text and cover 4 turns, got %+v", s.Context)
			}
		case prompt.KindToolUse:
			toolSummary = true
			if len(s.ToolUse.Lines) != 1 {
				t.Fatalf("only call_1 has a known id; got lines %+v", s.ToolUse.Lines)
			}
			if s.ToolUse.Lines[0].CallID != "call_1" {
				t.Fatalf("digest line = %+v", s.ToolUse.Lines[0])
			}
		}
	}
	if !ctxSummary || !toolSummary {
		t.Fatalf("want one context and one tool_use summary, got %+v", out)
	}

	// The digester must not see the newest round's output.
	if strings.Contains(fp.req.Messages[0].Parts[0].(provider.TextPart).Text, "read_file") {
		t.Fatal("the newest tool round was offered to the digester; it must stay verbatim")
	}
}

func TestModelCompactorSkipsSmallHistoryWithoutRounds(t *testing.T) {
	fp := &fakeProvider{name: "fake", reply: digesterReply}
	mc := &ModelCompactor{Provider: fp, Model: "small/fast"}

	out, err := mc.Compact(context.Background(), prompt.CompactionInput{
		Omitted: []domain.Turn{ // below MinTurns
			textTurn("t1", "user", "hello"),
			textTurn("t2", "agent", "hi"),
		},
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if out != nil || fp.calls != 0 {
		t.Fatalf("a two-turn tail is not worth a model call; out=%v calls=%d", out, fp.calls)
	}
}

func TestModelCompactorDigestsRoundsEvenWithSmallHistory(t *testing.T) {
	fp := &fakeProvider{name: "fake", reply: digesterReply}
	mc := &ModelCompactor{Provider: fp, Model: "small/fast"}

	out, err := mc.Compact(context.Background(), prompt.CompactionInput{
		ToolRounds: []prompt.ToolRound{
			{CallID: "call_1", Name: "shell", Output: "x", TurnRef: domain.TurnRef{ID: "r1", Version: 1}},
			{CallID: "call_2", Name: "shell", Output: "y", TurnRef: domain.TurnRef{ID: "r2", Version: 1}},
		},
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) != 1 || out[0].Kind != prompt.KindToolUse {
		t.Fatalf("want only the tool digest, got %+v", out)
	}
}

// --- DigestWorker ---

func TestDigestWorkerCommitsCommittedState(t *testing.T) {
	fp := &fakeProvider{name: "fake", reply: digesterReply}
	transcript := []domain.Turn{
		textTurn("t1", "user", strings.Repeat("z", 400)),
		textTurn("t2", "agent", strings.Repeat("y", 400)),
		textTurn("t3", "user", strings.Repeat("x", 400)),
		textTurn("t4", "agent", strings.Repeat("w", 400)),
		textTurn("t5", "user", strings.Repeat("v", 400)),
		textTurn("t6", "agent", strings.Repeat("u", 400)),
	}
	transcript = append(transcript, toolRoundTurns("call_1", "FAIL: TestSplit")...)
	transcript = append(transcript, toolRoundTurns("call_2", "ok")...)
	hist := &fakeHistory{snap: prompt.Snapshot{
		Conversation: domain.Conversation{ID: "c1", Owner: "u1"},
		Transcript:   transcript,
	}}
	w := &DigestWorker{History: hist, Compactor: &ModelCompactor{Provider: fp, Model: "small/fast"}, Pressure: 1e-9}

	out, err := w.MaybeCompact(context.Background(), kernel.Principal{Owner: "u1", Subject: "u1"}, "c1")
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want both summaries committed, got %+v", out)
	}
	if len(hist.appended) != 2 || len(hist.keys) != 2 {
		t.Fatalf("two turns must be committed, got %d / keys %d", len(hist.appended), len(hist.keys))
	}

	summary, digest := hist.appended[0], hist.appended[1]
	if summary.Actor.Kind != domain.ActorSystem || len(summary.Blocks) != 1 || summary.Blocks[0].Type != "summary" {
		t.Fatalf("the context summary must be a one-block system turn, got %+v", summary)
	}
	if !strings.HasPrefix(hist.keys[0], "summary:") {
		t.Fatalf("idempotency key = %q, want summary:<hash>", hist.keys[0])
	}
	if digest.Actor.Kind != domain.ActorSystem || len(digest.Blocks) != 1 || digest.Blocks[0].Type != "tool_digest" {
		t.Fatalf("the tool digest must be a tool_digest system turn, got %+v", digest)
	}
	if !strings.HasPrefix(hist.keys[1], "digest:") {
		t.Fatalf("idempotency key = %q, want digest:<hash>", hist.keys[1])
	}
}

func TestDigestWorkerNoOpsWhenNothingToDo(t *testing.T) {
	fp := &fakeProvider{name: "fake", reply: digesterReply}
	hist := &fakeHistory{snap: prompt.Snapshot{
		Conversation: domain.Conversation{ID: "c1", Owner: "u1"},
		Transcript:   []domain.Turn{textTurn("t1", "user", "hello")},
	}}
	w := &DigestWorker{History: hist, Compactor: &ModelCompactor{Provider: fp, Model: "small/fast"}}

	out, err := w.MaybeCompact(context.Background(), kernel.Principal{Owner: "u1", Subject: "u1"}, "c1")
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if out != nil || len(hist.appended) != 0 || fp.calls != 0 {
		t.Fatalf("nothing to compact must mean no calls and no writes; out=%v writes=%d calls=%d", out, len(hist.appended), fp.calls)
	}
}

func TestDigestWorkerSkipsCoveredHistory(t *testing.T) {
	// The transcript already carries a committed summary anchor; the worker
	// must not propose a second summary of the covered prefix.
	fp := &fakeProvider{name: "fake", reply: `{"context_summary": "duplicate"}`}
	hist := &fakeHistory{snap: prompt.Snapshot{
		Conversation: domain.Conversation{ID: "c1", Owner: "u1"},
		Transcript: []domain.Turn{
			summaryTurnFixture2("sum-1", "already summarized", []string{"t0:1"}),
			textTurn("t1", "user", "hello"),
		},
	}}
	w := &DigestWorker{History: hist, Compactor: &ModelCompactor{Provider: fp, Model: "small/fast"}, Pressure: 1e-9}

	if _, err := w.MaybeCompact(context.Background(), kernel.Principal{Owner: "u1", Subject: "u1"}, "c1"); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	for _, tn := range hist.appended {
		for _, blk := range tn.Blocks {
			if blk.Type == "summary" {
				t.Fatal("a second summary was committed despite an existing anchor")
			}
		}
	}
}

func summaryTurnFixture2(id, text string, covers []string) domain.Turn {
	payload := map[string]any{"text": text, "covers": covers, "origin": "compactor"}
	raw, _ := json.Marshal(payload)
	return domain.Turn{
		ID: id, Version: 1,
		Actor:  domain.Actor{Kind: domain.ActorSystem, ID: "workspace"},
		Blocks: []domain.Block{{Type: "summary", Value: payload, Raw: raw}},
	}
}
