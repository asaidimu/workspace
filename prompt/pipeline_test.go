package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

func textTurn(id, actor, text string) domain.Turn {
	raw, _ := json.Marshal(map[string]any{"text": text})
	return domain.Turn{
		ID:      id,
		Version: 1,
		Actor:   domain.Actor{Kind: domain.ActorKind(actor), ID: actor},
		Blocks:  []domain.Block{{Type: "text", Key: "k-" + id, Value: map[string]any{"text": text}, Raw: raw}},
	}
}

func toolCallTurn(id, callID, name string) domain.Turn {
	val := map[string]any{"call_id": callID, "name": name, "arguments": map[string]any{}}
	raw, _ := json.Marshal(val)
	return domain.Turn{
		ID:      id,
		Version: 1,
		Actor:   domain.Actor{Kind: domain.ActorAgent, ID: "agent"},
		Blocks:  []domain.Block{{Type: "tool_call", Key: "k-" + id, Value: val, Raw: raw}},
	}
}

func toolResultTurn(id, callID, out string) domain.Turn {
	val := map[string]any{"call_id": callID, "output": out, "is_error": false}
	raw, _ := json.Marshal(val)
	return domain.Turn{
		ID:      id,
		Version: 1,
		Actor:   domain.Actor{Kind: domain.ActorTool, ID: "tool"},
		Blocks:  []domain.Block{{Type: "tool_result", Key: "k-" + id, Value: val, Raw: raw}},
	}
}

func mkContent(key string, version int, mode domain.ContentMode, topics []string) domain.Content {
	val, _ := json.Marshal(map[string]any{"text": "body of " + key})
	return domain.Content{
		Key: key, Version: version, Owner: "u1", Type: "text", Mode: mode,
		Topics: topics, Value: val,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func mkPref(key string, version int) domain.Content {
	val, _ := json.Marshal(map[string]any{"name": "style", "text": "instruction of " + key})
	return domain.Content{
		Key: key, Version: version, Owner: "u1", Type: "preference", Mode: domain.ModeRead,
		Value:     val,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func baseInput() Input {
	return Input{
		Snapshot: Snapshot{
			Conversation: domain.Conversation{ID: "conv-1", Owner: "u1", Title: "t"},
			Transcript: []domain.Turn{
				textTurn("t1", "user", "hello"),
				textTurn("t2", "agent", "hi there"),
			},
		},
		Model:         "test/model",
		ContextWindow: 8000,
		Policy:        "Be concise.",
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	in := baseInput()
	in.Snapshot.Role = &domain.Role{ID: "r1", Persona: "You are terse."}
	in.Snapshot.Attached = []domain.Content{
		mkContent("ctx1", 1, domain.ModeRead, []string{"a"}),
		mkContent("art1", 2, domain.ModeReadWrite, []string{"b"}),
	}
	in.Snapshot.Settings.GlobalPreferenceKeys = []store.ContentRef{{Key: "pref1", Version: 1}}
	in.Snapshot.Attached = append(in.Snapshot.Attached, mkPref("pref1", 1))
	in.Instructions = "Answer briefly."
	in.Tools = []provider.ToolSpec{{Name: "search", Description: "find things"}}

	first, d1, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	second, d2, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("two builds of the same input differ:\n%s\n---\n%s", a, b)
	}
	if len(d1) != len(d2) {
		t.Fatalf("diagnostics differ: %v vs %v", d1, d2)
	}
}

func TestSectionsFollowSpecOrder(t *testing.T) {
	in := baseInput()
	in.Snapshot.Role = &domain.Role{ID: "r1", Persona: "Persona text."}
	in.Snapshot.Attached = []domain.Content{
		mkContent("ctx1", 1, domain.ModeRead, nil),
		mkContent("art1", 1, domain.ModeReadWrite, nil),
		mkPref("pref1", 1),
	}
	in.Snapshot.Role.PreferenceKeys = []store.ContentRef{{Key: "pref1", Version: 1}}
	in.Instructions = "Do the thing."

	// Read the assembled section IDs through a capture stage.
	var got []string
	capture := captureStage{name: "capture", fn: func(ctx context.Context, b *Build) error {
		for _, s := range b.Sections {
			got = append(got, s.ID)
		}
		return nil
	}}
	p := NewPipeline(snapshotStage{}, selectStage{}, resolveStage{}, assembleStage{}, capture)
	if _, _, err := p.Build(context.Background(), in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{"persona", "policy", "preferences", "context", "artifacts", "instructions"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("section order = %v, want %v", got, want)
	}
}

func TestNativeModePutsToolsOnTheRequestNotASection(t *testing.T) {
	in := baseInput()
	in.Tools = []provider.ToolSpec{{Name: "search", Description: "find things"}}

	req, _, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "search" {
		t.Fatalf("native mode must attach tools to the request, got %+v", req.Tools)
	}
	for _, s := range req.System {
		if txt, ok := s.(provider.TextPart); ok && strings.Contains(txt.Text, "## search") {
			t.Fatal("native mode must not describe tools in a system section")
		}
	}
}

func TestStructuredModeAttachesSchemaAndSkipsRequestTools(t *testing.T) {
	in := baseInput()
	in.Structured = true
	in.OutputSchema = json.RawMessage(`{"type":"object"}`)
	in.Tools = []provider.ToolSpec{{Name: "search", Description: "find things"}}

	req, _, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if string(req.OutputSchema) != `{"type":"object"}` {
		t.Fatalf("structured mode must attach the schema, got %s", req.OutputSchema)
	}
	if len(req.Tools) != 0 {
		t.Fatalf("structured mode describes tools in a section, not the request: %+v", req.Tools)
	}
	// The tools section must now exist.
	found := false
	for _, s := range req.System {
		if txt, ok := s.(provider.TextPart); ok && strings.Contains(txt.Text, "## search") {
			found = true
		}
	}
	if !found {
		t.Fatal("structured mode must describe tools in a system section")
	}
}

func TestFixedSectionsOverBudgetFails(t *testing.T) {
	in := baseInput()
	in.Policy = strings.Repeat("x", 4000) // 1000 tokens, plus the title
	in.ContextWindow = 1100               // reserved 110, window 990

	_, _, err := DefaultPipeline().Build(context.Background(), in)
	if !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
	if !strings.Contains(err.Error(), "fixed sections") {
		t.Fatalf("message should name the cause: %v", err)
	}
}

func TestTranscriptIsFittedNewestFirst(t *testing.T) {
	in := baseInput()
	// The transcript is chronological, so the appended turns are the newest.
	// A moderate window keeps the newest few and omits the oldest.
	for i := 0; i < 8; i++ {
		in.Snapshot.Transcript = append(in.Snapshot.Transcript,
			textTurn("old"+itoa(i), "user", "OLD"+itoa(i)+strings.Repeat("y", 396)))
	}
	// Window 216: fixed sections cost ~5, leaving ~211 for the transcript.
	// Newest first that holds the two newest bulky turns (~202).
	in.ContextWindow = 240

	req, _, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(req.Messages) == 0 {
		t.Fatal("nothing survived the budget")
	}
	last := req.Messages[len(req.Messages)-1]
	parts, ok := last.Parts[0].(provider.TextPart)
	if !ok || !strings.Contains(parts.Text, "OLD7") {
		t.Fatalf("the newest turn must survive: %+v", last)
	}
	for _, m := range req.Messages {
		p, ok := m.Parts[0].(provider.TextPart)
		if !ok {
			continue
		}
		if strings.Contains(p.Text, "hello") || strings.Contains(p.Text, "hi there") {
			t.Fatal("the oldest turns should have been omitted first")
		}
		if strings.Contains(p.Text, "OLD0") {
			t.Fatal("OLD0 is older than the surviving turns and must be omitted")
		}
	}
}

func TestToolCallAndResultAreKeptTogether(t *testing.T) {
	in := baseInput()
	in.Snapshot.Transcript = append(in.Snapshot.Transcript,
		toolCallTurn("t3", "call-1", "search"),
		toolResultTurn("t4", "call-1", "a result"),
		textTurn("t5", "agent", "done"),
	)
	// A budget that can hold the last two turns but not the whole history.
	in.ContextWindow = 1600

	req, _, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var sawCall, sawResult bool
	var callIdx, resultIdx = -1, -1
	for i, m := range req.Messages {
		for _, p := range m.Parts {
			switch v := p.(type) {
			case provider.ToolCallPart:
				sawCall = true
				if v.CallID == "call-1" {
					callIdx = i
				}
			case provider.ToolResultPart:
				sawResult = true
				if v.CallID == "call-1" {
					resultIdx = i
				}
			}
		}
	}
	if sawCall != sawResult {
		t.Fatalf("call and result were split: call=%v result=%v", sawCall, sawResult)
	}
	if callIdx >= 0 && resultIdx < 0 {
		t.Fatal("the call survived but its result did not")
	}
	if resultIdx >= 0 && callIdx < 0 {
		t.Fatal("the result survived but its call did not")
	}
	if callIdx >= 0 && resultIdx < callIdx {
		t.Fatal("the tool result must not appear before its call")
	}
}

func TestOmissionDropsOldestAndRecordsDiagnostic(t *testing.T) {
	in := baseInput()
	for i := 0; i < 20; i++ {
		in.Snapshot.Transcript = append(in.Snapshot.Transcript,
			textTurn("old"+itoa(i), "user", strings.Repeat("z", 400)))
	}
	in.ContextWindow = 2000

	req, diags, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(req.Messages) == 0 {
		t.Fatal("nothing survived")
	}
	sawLast := false
	for _, m := range req.Messages {
		if tp, ok := m.Parts[0].(provider.TextPart); ok && tp.Text == strings.Repeat("z", 400) {
			sawLast = true
		}
	}
	if !sawLast {
		t.Fatal("the newest turns must survive the fit")
	}
	var sawOmitted bool
	for _, d := range diags {
		if d.Code == "transcript.omitted" {
			sawOmitted = true
		}
	}
	if !sawOmitted {
		t.Fatalf("dropping turns to fit must be recorded as transcript.omitted; got %v", diags)
	}
}

func summaryTurnFixture(id, text string, covers []string) domain.Turn {
	payload := map[string]any{"text": text, "covers": covers, "origin": "compactor"}
	raw, _ := json.Marshal(payload)
	return domain.Turn{
		ID:      id,
		Version: 1,
		Actor:   domain.Actor{Kind: domain.ActorSystem, ID: "workspace"},
		Blocks:  []domain.Block{{Type: "summary", Value: payload, Raw: raw}},
	}
}

func TestCommittedSummaryAnchorsTheFit(t *testing.T) {
	in := baseInput()
	// A long prefix, a committed summary turn covering it, then a short tail.
	for i := 0; i < 20; i++ {
		in.Snapshot.Transcript = append(in.Snapshot.Transcript,
			textTurn("old"+itoa(i), "user", strings.Repeat("z", 400)))
	}
	in.Snapshot.Transcript = append(in.Snapshot.Transcript,
		summaryTurnFixture("sum-1", "SUMMARY OF THE PAST", []string{"old0:1", "old19:1"}),
		textTurn("recent", "user", "the actual question"),
	)
	in.ContextWindow = 2000

	req, _, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(req.Messages) == 0 {
		t.Fatal("nothing survived")
	}
	first, ok := req.Messages[0].Parts[0].(provider.TextPart)
	if !ok || !strings.Contains(first.Text, "SUMMARY OF THE PAST") {
		t.Fatalf("the committed summary must lead the transcript, got %+v", req.Messages[0])
	}
	if req.Messages[0].Role != provider.UserRole {
		t.Fatalf("a summary is a system turn, mapped to user-level context; role = %v", req.Messages[0].Role)
	}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if tp, ok := p.(provider.TextPart); ok && strings.Repeat("z", 400) == tp.Text {
				t.Fatal("a covered turn was re-read into the prompt; the anchor must suppress its prefix")
			}
		}
	}
}

func TestCommittedToolDigestsReplaceOldBodies(t *testing.T) {
	in := baseInput()
	callBlock := func(id string) domain.Block {
		return domain.Block{Type: "tool_call", Value: map[string]any{
			"call_id": id, "name": "shell", "arguments": json.RawMessage(`{"command":"go test"}`)}}
	}
	resultBlock := func(id, out string) domain.Block {
		return domain.Block{Type: "tool_result", Value: map[string]any{
			"call_id": id, "output": out}}
	}
	in.Snapshot.Transcript = []domain.Turn{
		textTurn("t1", "user", "run the tests"),
		{ID: "t2", Version: 1, Actor: domain.Actor{Kind: domain.ActorAgent, ID: "a"},
			Blocks: []domain.Block{callBlock("call_1")}},
		{ID: "t3", Version: 1, Actor: domain.Actor{Kind: domain.ActorTool, ID: "shell"},
			Blocks: []domain.Block{resultBlock("call_1", strings.Repeat("FAIL", 500))}},
		{ID: "t4", Version: 1, Actor: domain.Actor{Kind: domain.ActorAgent, ID: "a"},
			Blocks: []domain.Block{callBlock("call_2")}},
		{ID: "t5", Version: 1, Actor: domain.Actor{Kind: domain.ActorTool, ID: "shell"},
			Blocks: []domain.Block{resultBlock("call_2", "ok")}},
	}
	digestPayload := map[string]any{"call_id": "call_1", "text": "Weather check completed for Lisbon: 19 C"}
	raw, _ := json.Marshal(digestPayload)
	in.Snapshot.Transcript = append(in.Snapshot.Transcript, domain.Turn{
		ID: "t6", Version: 1, Actor: domain.Actor{Kind: domain.ActorSystem, ID: "workspace"},
		Blocks: []domain.Block{{Type: "tool_digest", Value: digestPayload, Raw: raw}},
	})

	req, _, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var digested, verbatim, sawDigestBlock int
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			switch tp := p.(type) {
			case provider.ToolResultPart:
				if strings.Contains(string(tp.Output), "Weather check completed") {
					digested++
				}
				if strings.Contains(string(tp.Output), strings.Repeat("FAIL", 500)) {
					t.Fatal("a digested round's raw body was still sent")
				}
				if string(tp.Output) == `"ok"` || strings.Contains(string(tp.Output), "ok") {
					verbatim++
				}
			case provider.TextPart:
				if tp.Text == "Weather check completed for Lisbon: 19 C" {
					t.Fatal("a tool_digest block leaked into the prompt as text")
				}
			}
		}
	}
	for _, t := range in.Snapshot.Transcript {
		for _, blk := range t.Blocks {
			if blk.Type == "tool_digest" {
				sawDigestBlock++
			}
		}
	}
	if digested != 1 {
		t.Fatalf("exactly the old round must be digested; got %d", digested)
	}
	if verbatim != 1 {
		t.Fatalf("the newest round must stay verbatim; got %d", verbatim)
	}
	if sawDigestBlock != 1 {
		t.Fatalf("the fixture is broken: digest blocks = %d", sawDigestBlock)
	}
}

func TestEstimatorRecordsApproximateDiagnostic(t *testing.T) {
	_, diags, err := DefaultPipeline().Build(context.Background(), baseInput())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, d := range diags {
		if d.Code == "token.approximate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the default tokenizer must mark its counts approximate; got %v", diags)
	}
}

func TestCustomTokenizerSuppressesApproximateDiagnostic(t *testing.T) {
	in := baseInput()
	in.Tokenizer = TokenizerFunc(func(model string, parts []provider.Part) (int, error) { return 1, nil })
	_, diags, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, d := range diags {
		if d.Code == "token.approximate" {
			t.Fatal("a supplied tokenizer is not approximate by definition")
		}
	}
}

func TestResolveDropsTombstonedAndRecordsConflict(t *testing.T) {
	in := baseInput()
	tomb := mkContent("dead", 3, domain.ModeRead, nil)
	tomb.Tombstoned = true
	in.Snapshot.Attached = []domain.Content{
		mkContent("ctx1", 1, domain.ModeRead, nil),
		tomb,
	}
	// Two sources for the same key, at different versions: a conflict.
	in.Snapshot.Settings.GlobalPreferenceKeys = []store.ContentRef{{Key: "ctx1", Version: 1}}
	in.Snapshot.Attached = append(in.Snapshot.Attached, mkContent("ctx1", 2, domain.ModeRead, nil))

	var got []Diagnostic
	capture := captureStage{name: "capture", fn: func(ctx context.Context, b *Build) error {
		got = append(got, b.Diagnostics...)
		return nil
	}}
	p := NewPipeline(snapshotStage{}, selectStage{}, resolveStage{}, capture)
	if _, _, err := p.Build(context.Background(), in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var sawTomb, sawConflict bool
	for _, d := range got {
		switch d.Code {
		case "content.tombstoned":
			sawTomb = true
		case "content.conflict":
			sawConflict = true
		}
	}
	if !sawTomb {
		t.Fatalf("a tombstoned key must be dropped with a diagnostic; got %v", got)
	}
	if !sawConflict {
		t.Fatalf("two sources for one key must record a conflict; got %v", got)
	}
}

func TestResolveReportsUnretrievableCandidate(t *testing.T) {
	in := baseInput()
	// The retriever names a key the snapshot never carried.
	in.Retriever = retrieverFunc(func(ctx context.Context, q RetrievalQuery) ([]Scored, error) {
		return []Scored{{Ref: store.ContentRef{Key: "ghost", Version: 1}, Score: 0.9}}, nil
	})
	var got []Diagnostic
	capture := captureStage{name: "capture", fn: func(ctx context.Context, b *Build) error {
		got = append(got, b.Diagnostics...)
		return nil
	}}
	p := NewPipeline(snapshotStage{}, selectStage{}, resolveStage{}, capture)
	if _, _, err := p.Build(context.Background(), in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, d := range got {
		if d.Code == "content.unresolved" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a retriever hit with no content must be reported; got %v", got)
	}
}

func TestPrecedenceAttachmentBeatsRetriever(t *testing.T) {
	in := baseInput()
	att := mkContent("shared", 1, domain.ModeRead, []string{"a"})
	in.Snapshot.Attached = []domain.Content{att}
	in.Retriever = retrieverFunc(func(ctx context.Context, q RetrievalQuery) ([]Scored, error) {
		return []Scored{{Ref: store.ContentRef{Key: "shared", Version: 1}, Score: 0.99}}, nil
	})

	var kinds []CandidateKind
	capture := captureStage{name: "capture", fn: func(ctx context.Context, b *Build) error {
		for _, c := range b.Candidates {
			kinds = append(kinds, c.Kind)
		}
		var resolved []resolvedItem
		resolved = append(resolved, b.resolved...)
		for _, r := range resolved {
			if r.Content.Key == "shared" && r.Kind != FromAttachment {
				t.Errorf("shared resolved from %v, want FromAttachment", r.Kind)
			}
		}
		return nil
	}}
	p := NewPipeline(snapshotStage{}, selectStage{}, resolveStage{}, capture)
	if _, _, err := p.Build(context.Background(), in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(kinds) < 2 {
		t.Fatalf("both an attachment and a retrieval hit should be candidates, got %v", kinds)
	}
}

func TestNilSnapshotIsRejected(t *testing.T) {
	_, _, err := DefaultPipeline().Build(context.Background(), Input{})
	if !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
}

func TestBuildHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := DefaultPipeline().Build(ctx, baseInput())
	if !kernel.IsCode(err, kernel.CodeCanceled) {
		t.Fatalf("err = %v, want CANCELED", err)
	}
}

func TestRetrieverErrorSurfaces(t *testing.T) {
	in := baseInput()
	in.Retriever = retrieverFunc(func(ctx context.Context, q RetrievalQuery) ([]Scored, error) {
		return nil, errors.New("index down")
	})
	_, _, err := DefaultPipeline().Build(context.Background(), in)
	if !kernel.IsCode(err, kernel.CodeUnavailable) {
		t.Fatalf("err = %v, want UNAVAILABLE", err)
	}
}

func TestEstimatorChargesBytesDividedByFour(t *testing.T) {
	cases := []struct {
		part provider.Part
		want int
	}{
		{provider.TextPart{Text: ""}, 1},
		{provider.TextPart{Text: "abcd"}, 1},
		{provider.TextPart{Text: "abcde"}, 2},
		{provider.ToolCallPart{Name: "search", Arguments: json.RawMessage(`{}`)}, 2},
		{provider.ToolResultPart{Output: json.RawMessage(`{"a":1}`)}, 2}, // 7 bytes → 2
	}
	for _, c := range cases {
		got, err := (Estimator{}).Count("m", []provider.Part{c.part})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if got != c.want {
			t.Errorf("Count(%#v) = %d, want %d", c.part, got, c.want)
		}
	}
}

func TestTopicRetrieverRanksByOverlapThenRecency(t *testing.T) {
	older := mkContent("older", 1, domain.ModeRead, []string{"go", "store"})
	older.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := mkContent("newer", 1, domain.ModeRead, []string{"go", "store"})
	newer.CreatedAt = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	weak := mkContent("weak", 1, domain.ModeRead, []string{"go", "rust", "java", "c"})
	dead := mkContent("dead", 1, domain.ModeRead, []string{"go"})
	dead.Tombstoned = true
	other := mkContent("other", 1, domain.ModeRead, []string{"go"})
	other.Owner = "u2"

	r := TopicRetriever{Items: []domain.Content{older, newer, weak, dead, other}}
	got, err := r.Retrieve(context.Background(), RetrievalQuery{
		Owner: "u1", Topics: []string{"go", "store"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d hits, want 3 (tombstoned and other-owner filtered)", len(got))
	}
	if got[0].Ref.Key != "newer" || got[1].Ref.Key != "older" || got[2].Ref.Key != "weak" {
		t.Fatalf("order = %s,%s,%s; want newer,older,weak", got[0].Ref.Key, got[1].Ref.Key, got[2].Ref.Key)
	}
}

func TestTopicRetrieverHonoursLimitAndCancelledContext(t *testing.T) {
	var items []domain.Content
	for i := 0; i < 30; i++ {
		c := mkContent("c"+itoa(i), 1, domain.ModeRead, []string{"go"})
		items = append(items, c)
	}
	r := TopicRetriever{Items: items}
	got, err := r.Retrieve(context.Background(), RetrievalQuery{Owner: "u1", Topics: []string{"go"}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("default limit = %d, want 20", len(got))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Retrieve(ctx, RetrievalQuery{Owner: "u1"}); err == nil {
		t.Fatal("a cancelled context must be reported")
	}
}

// --- local doubles ---

type stageFunc string

func (f stageFunc) Name() string                            { return string(f) }
func (f stageFunc) Run(ctx context.Context, b *Build) error { return f.run(ctx, b) }

// run is separate so the method set stays one line per stage and the func
// value can be compared by name only.
func (f stageFunc) run(ctx context.Context, b *Build) error { return nil }

// captureStage is a Stage built from a function, used to inspect build state.
type captureStage struct {
	name string
	fn   func(ctx context.Context, b *Build) error
}

func (s captureStage) Name() string                            { return s.name }
func (s captureStage) Run(ctx context.Context, b *Build) error { return s.fn(ctx, b) }

type retrieverFunc func(ctx context.Context, q RetrievalQuery) ([]Scored, error)

func (f retrieverFunc) Retrieve(ctx context.Context, q RetrievalQuery) ([]Scored, error) {
	return f(ctx, q)
}

type TokenizerFunc func(model string, parts []provider.Part) (int, error)

func (f TokenizerFunc) Count(model string, parts []provider.Part) (int, error) {
	return f(model, parts)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestStageNames(t *testing.T) {
	want := map[string]string{
		"snapshot": "snapshot", "select": "select", "resolve": "resolve",
		"compact": "compact", "assemble": "assemble", "map": "map",
	}
	stages := []Stage{
		snapshotStage{}, selectStage{}, resolveStage{},
		compactStage{}, assembleStage{}, mapStage{},
	}
	for _, st := range stages {
		if want[st.Name()] != st.Name() {
			t.Errorf("stage %T has name %q", st, st.Name())
		}
		if got := st.Name(); got == "" {
			t.Errorf("stage %T has an empty name", st)
		}
	}
	if DefaultPipeline() == nil {
		t.Fatal("DefaultPipeline must not be nil")
	}
}

func TestFitUnitsWithNoBudgetOmitsEverything(t *testing.T) {
	turns := []domain.Turn{textTurn("a", "user", "x")}
	cost := func([]domain.Turn) (int, error) { return 0, nil }
	kept, omitted := fitUnits(pairToolCalls(turns), cost, 0)
	if len(kept) != 0 || len(omitted) != 1 {
		t.Fatalf("kept=%d omitted=%d, want all omitted", len(kept), len(omitted))
	}
}

func TestEstimatorChargesFlatCostForUnknownParts(t *testing.T) {
	got, err := (Estimator{}).Count("m", []provider.Part{provider.ImagePart{}})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 4 {
		t.Fatalf("an unknown part costs a flat 4, got %d", got)
	}
}

func TestBetterCandidatePrefersHigherScoreInRetrieverTier(t *testing.T) {
	lo := Candidate{Ref: store.ContentRef{Key: "k", Version: 1}, Kind: FromRetriever, Score: 0.1}
	hi := Candidate{Ref: store.ContentRef{Key: "k", Version: 1}, Kind: FromRetriever, Score: 0.9}
	if !betterCandidate(hi, lo) {
		t.Fatal("a higher retrieval score must win")
	}
	if betterCandidate(lo, hi) {
		t.Fatal("a lower retrieval score must not win")
	}
}

func TestPairToolCallWithoutResultIsItsOwnUnit(t *testing.T) {
	units := pairToolCalls([]domain.Turn{toolCallTurn("t1", "call-1", "search")})
	if len(units) != 1 || len(units[0]) != 1 {
		t.Fatalf("units = %d (first has %d), want one unit of one turn", len(units), len(units[0]))
	}
}

func TestTurnHasToolResultRejectsOtherCalls(t *testing.T) {
	if turnHasToolResult(toolResultTurn("t1", "call-2", "out"), "call-1") {
		t.Fatal("a result for another call must not match")
	}
	if !turnHasToolResult(toolResultTurn("t1", "call-1", "out"), "call-1") {
		t.Fatal("a result with the matching call ID must match")
	}
}

func TestConstraintsMergeRoleConversationCaller(t *testing.T) {
	temp := func(v float64) *float64 { return &v }
	maxTok := func(v int) *int { return &v }

	in := baseInput()
	in.Snapshot.Role = &domain.Role{
		ID: "r1", Persona: "p",
		Constraints: json.RawMessage(`{"temperature":0.2,"stop":["role-stop"]}`),
	}
	in.Snapshot.Conversation.Constraints = json.RawMessage(`{"temperature":0.5,"max_output_tokens":200}`)
	in.Constraints = &domain.Constraints{Temperature: temp(0.9)}

	req, _, err := DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0.9 {
		t.Fatalf("temperature = %v, want the caller's 0.9", req.Temperature)
	}
	if req.MaxOutputTokens != 200 {
		t.Errorf("max output tokens = %d, want the conversation's 200", req.MaxOutputTokens)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "role-stop" {
		t.Errorf("stop = %v, want the role's stop (nobody later overrode it)", req.Stop)
	}

	// A later source that sets a field wins for that field only.
	in.Constraints = &domain.Constraints{Stop: []string{"caller-stop"}, MaxOutputTokens: maxTok(50)}
	req, _, err = DefaultPipeline().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if *req.Temperature != 0.5 {
		t.Errorf("temperature = %v, want the conversation's 0.5", req.Temperature)
	}
	if req.MaxOutputTokens != 50 {
		t.Errorf("max output tokens = %d, want the caller's 50", req.MaxOutputTokens)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "caller-stop" {
		t.Errorf("stop = %v, want the caller's", req.Stop)
	}
}
