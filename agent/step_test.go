package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/prompt"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/workspacetest"
)

// timeline is one ordered record every double writes into, so the relative
// order of a history append and a tool execution is observable. Two separate
// logs concatenated after the fact would only prove each ran, not when.
type timeline struct {
	mu     sync.Mutex
	events []string
}

func (t *timeline) add(ev string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, ev)
}

func (t *timeline) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.events))
	copy(out, t.events)
	return out
}

// fakeHistory is the step's durable-state port. It records the order of
// reads and appends, which is what the commit-order rule is tested against.
type fakeHistory struct {
	mu        sync.Mutex
	snapshot  prompt.Snapshot
	line      *timeline
	turns     []domain.Turn
	readErr   error
	appendErr error
	// failFrom, when non-zero, refuses every append once that many turns
	// have already committed, so a test can commit the assistant turn and
	// have the tool results refused.
	failFrom int
}

func newHistory(line *timeline) *fakeHistory {
	head := &domain.TurnRef{ID: "head0", Version: 1}
	return &fakeHistory{line: line, snapshot: prompt.Snapshot{
		Conversation: domain.Conversation{ID: "c1", Owner: "u1", Rev: 1, Head: head},
	}}
}

func (h *fakeHistory) Read(ctx context.Context, p kernel.Principal, conversationID string) (prompt.Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.line.add("read:" + conversationID)
	if h.readErr != nil {
		return prompt.Snapshot{}, h.readErr
	}
	return h.snapshot, nil
}

func (h *fakeHistory) Append(ctx context.Context, p kernel.Principal, conversationID string, turn domain.Turn, key string) (domain.Turn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.appendErr != nil {
		return domain.Turn{}, h.appendErr
	}
	if h.failFrom > 0 && len(h.turns) >= h.failFrom {
		return domain.Turn{}, kernel.NewError(kernel.CodeBusy, "append refused")
	}
	h.line.add("append:" + string(turn.Actor.Kind))
	h.turns = append(h.turns, turn)
	return turn, nil
}

// failAfter makes appends fail from the n-th one on.
func (h *fakeHistory) failAfter(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failFrom = n
}

func (h *fakeHistory) committed() []domain.Turn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]domain.Turn, len(h.turns))
	copy(out, h.turns)
	return out
}

// recorder is what a test tool writes into: the events a tool observed, in
// order, plus the principal it ran under.
type recorder struct {
	mu         sync.Mutex
	line       *timeline
	principals map[string]kernel.Principal
	arrived    chan struct{}
	release    chan struct{}
}

func newRecorder(line *timeline) *recorder {
	return &recorder{line: line, principals: map[string]kernel.Principal{}}
}

func (r *recorder) add(ev string) { r.line.add(ev) }
func (r *recorder) all() []string { return r.line.all() }

// argTool is the input type tools in these tests accept. Its one field is
// optional so `{}` stays a valid argument payload with the schema checks on.
type argTool struct {
	Arg string `json:"arg,omitempty"`
}

// countingTool is a tool that records that it ran and under which principal.
func countingTool(r *recorder, spec agent.ToolSpec) agent.Tool {
	return agent.NewTool(spec, func(ctx context.Context, p kernel.Principal, in argTool) (map[string]string, error) {
		r.add("tool:" + spec.Name)
		r.mu.Lock()
		r.principals[spec.Name] = p
		r.mu.Unlock()
		return map[string]string{"ran": spec.Name}, nil
	})
}

// barrierTool blocks until `cap` tools have arrived, so a test can prove
// calls really do run in parallel rather than merely being started together.
func barrierTool(r *recorder, spec agent.ToolSpec) agent.Tool {
	return agent.NewTool(spec, func(ctx context.Context, p kernel.Principal, in argTool) (map[string]string, error) {
		r.arrived <- struct{}{}
		<-r.release
		r.add("tool:" + spec.Name)
		return map[string]string{"ran": spec.Name}, nil
	})
}

// failingTool always returns an error the model should see.
func failingTool(r *recorder, spec agent.ToolSpec) agent.Tool {
	return agent.NewTool(spec, func(ctx context.Context, p kernel.Principal, in argTool) (map[string]string, error) {
		r.add("tool:" + spec.Name)
		return nil, fmt.Errorf("boom: %s", in.Arg)
	})
}

type stepFixture struct {
	line     *timeline
	hist     *fakeHistory
	rec      *recorder
	prov     *workspacetest.ScriptedProvider
	registry agent.Registry
	stepper  agent.Stepper
	clock    *workspacetest.FakeClock
	ids      *workspacetest.SeqIDs
	pricer   agent.Pricer
}

func newStepFixture(t *testing.T) *stepFixture {
	t.Helper()
	return newStepFixtureWith(t, func(m string, u provider.Usage) int64 {
		return int64(u.InputTokens+u.OutputTokens) * 10
	})
}

// newStepFixtureWith builds the same fixture with a caller-chosen pricer, so
// a test can make a run spend money without a provider reporting usage.
func newStepFixtureWith(t *testing.T, pricer agent.Pricer) *stepFixture {
	t.Helper()
	f := &stepFixture{
		line: &timeline{},
	}
	f.hist = newHistory(f.line)
	f = &stepFixture{
		line:     f.line,
		hist:     newHistory(f.line),
		rec:      newRecorder(f.line),
		prov:     workspacetest.NewScriptedProvider("scripted"),
		registry: agent.NewRegistry(),
		clock:    workspacetest.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		ids:      workspacetest.NewSeqIDs("id"),
	}
	s, err := agent.NewStepper(agent.StepperConfig{
		History:  f.hist,
		Pipeline: prompt.DefaultPipeline(),
		Provider: f.prov,
		Registry: f.registry,
		Pricer:   pricer,
		Clock:    f.clock,
		IDs:      f.ids,
	})
	if err != nil {
		t.Fatalf("agent.NewStepper: %v", err)
	}
	f.stepper = s
	return f
}

func (f *stepFixture) register(t *testing.T, tools ...agent.Tool) {
	t.Helper()
	for _, tool := range tools {
		if err := f.registry.Register(tool); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
}

func testPrincipal() kernel.Principal {
	return kernel.Principal{Owner: "u1", Subject: "u1", Kind: kernel.UserPrincipal, Scopes: []string{"fs:read", "other:scope"}}
}

func TestNewStepperRejectsIncompleteConfig(t *testing.T) {
	base := func(c *agent.StepperConfig) {
		c.History = newHistory(&timeline{})
		c.Pipeline = prompt.DefaultPipeline()
		c.Provider = workspacetest.NewScriptedProvider("p")
		c.Registry = agent.NewRegistry()
		c.Clock = workspacetest.NewFakeClock(time.Now())
		c.IDs = workspacetest.NewSeqIDs("id")
	}
	cases := []struct {
		name string
		mut  func(*agent.StepperConfig)
	}{
		{"no history", func(c *agent.StepperConfig) { c.History = nil }},
		{"no pipeline", func(c *agent.StepperConfig) { c.Pipeline = nil }},
		{"no provider", func(c *agent.StepperConfig) { c.Provider = nil }},
		{"no registry", func(c *agent.StepperConfig) { c.Registry = nil }},
		{"no clock", func(c *agent.StepperConfig) { c.Clock = nil }},
		{"no ids", func(c *agent.StepperConfig) { c.IDs = nil }},
	}
	for _, c := range cases {
		cfg := agent.StepperConfig{}
		base(&cfg)
		c.mut(&cfg)
		if _, err := agent.NewStepper(cfg); !kernel.IsCode(err, kernel.CodeInvalid) {
			t.Errorf("%s: err = %v, want INVALID", c.name, err)
		}
	}
}

func TestStepCommitsTheAssistantTurn(t *testing.T) {
	f := newStepFixture(t)
	f.prov.ReplyText("hello")

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(res.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(res.Turns))
	}
	turn := res.Turns[0]
	if turn.Actor.Kind != domain.ActorAgent {
		t.Errorf("actor = %v, want agent", turn.Actor.Kind)
	}
	if turn.Parent == nil || turn.Parent.ID != "head0" {
		t.Errorf("parent = %v, want the snapshot head", turn.Parent)
	}
	if turn.FinishReason != string(provider.FinishStop) {
		t.Errorf("finish = %q", turn.FinishReason)
	}
	if len(turn.Blocks) != 1 || turn.Blocks[0].Type != "text" {
		t.Fatalf("blocks = %+v", turn.Blocks)
	}
	if turn.CreatedAt.IsZero() {
		t.Error("the injected clock must stamp the turn")
	}
	if res.Finish != provider.FinishStop {
		t.Errorf("res.Finish = %v", res.Finish)
	}
	if len(res.Pending) != 0 {
		t.Errorf("pending = %+v", res.Pending)
	}
}

func TestStepCommitsTheAssistantTurnBeforeRunningTools(t *testing.T) {
	f := newStepFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "search"}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call1", Name: "search", Arguments: json.RawMessage(`{"arg":"q"}`)})

	_, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	events := f.line.all()
	want := []string{
		"read:c1",
		"append:agent",
		"tool:search",
		"append:tool",
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %q, want %q (full: %v)", i, events[i], want[i], events)
		}
	}
}

// recordedEffect is one dispatched effect command plus its idempotency key.
type recordedEffect struct {
	name string
	key  string
}

// testEffectCommand is a minimal kernel command the note effect yields.
type testEffectCommand struct{}

func (testEffectCommand) CommandName() string { return "test:effect" }

// dispatchRecorder is the agent.EffectsDispatcher fake; it records names and
// keys so a test can prove block-bound keying.
type dispatchRecorder struct{ got *[]recordedEffect }

func (r dispatchRecorder) DispatchEffect(ctx context.Context, p kernel.Principal, cmd kernel.Command, key string) error {
	if cmd == nil {
		return errors.New("nil command")
	}
	*r.got = append(*r.got, recordedEffect{name: cmd.CommandName(), key: key})
	return nil
}

// noteEffects is the content.Effects fake for the note type: one command.
type noteEffects struct{}

func (noteEffects) Effects(ctx context.Context, b domain.Block) ([]kernel.Command, error) {
	if strings.Contains(fmt.Sprint(b.Value), "fail") {
		return nil, errors.New("effect build failed")
	}
	return []kernel.Command{testEffectCommand{}}, nil
}

func TestStepDispatchesBlockEffects(t *testing.T) {
	f := newStepFixture(t)
	reg := content.NewRegistry()
	note, err := content.NewBlockType[struct {
		Text string `json:"text"`
	}]("note", content.Consumable|content.Emittable, content.WithDescription("A note"))
	if err != nil {
		t.Fatalf("NewBlockType: %v", err)
	}
	var got []recordedEffect
	if err := reg.Register(note, content.WithEffects(noteEffects{})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s, err := agent.NewStepper(agent.StepperConfig{
		History:  f.hist,
		Pipeline: prompt.DefaultPipeline(),
		Provider: f.prov,
		Registry: f.registry,
		Pricer: func(m string, u provider.Usage) int64 {
			return int64(u.InputTokens+u.OutputTokens) * 10
		},
		Clock:   f.clock,
		IDs:     f.ids,
		Content: reg,
		Effects: dispatchRecorder{got: &got},
	})
	if err != nil {
		t.Fatalf("NewStepper: %v", err)
	}
	f.prov.ReplyText("do it\n```workspace:note\n{\"text\":\"dispatch me\"}\n```\n")

	res, err := s.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("dispatched %d effects, want 1", len(got))
	}
	if got[0].name != "test:effect" {
		t.Errorf("effect name = %q, want test:effect", got[0].name)
	}
	// The prose before the fence commits as its own text block, so the note
	// lands at index 1.
	if want := "turn:" + res.Turns[0].ID + ".1:1:0"; got[0].key != want {
		t.Errorf("idempotency key = %q, want %q", got[0].key, want)
	}
}

func TestStepFailsWhenAnEffectDispatchFails(t *testing.T) {
	f := newStepFixture(t)
	reg := content.NewRegistry()
	note, err := content.NewBlockType[struct {
		Text string `json:"text"`
	}]("note", content.Consumable|content.Emittable, content.WithDescription("A note"))
	if err != nil {
		t.Fatalf("NewBlockType: %v", err)
	}
	var got []recordedEffect
	if err := reg.Register(note, content.WithEffects(noteEffects{})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s, err := agent.NewStepper(agent.StepperConfig{
		History:  f.hist,
		Pipeline: prompt.DefaultPipeline(),
		Provider: f.prov,
		Registry: f.registry,
		Pricer: func(m string, u provider.Usage) int64 {
			return int64(u.InputTokens+u.OutputTokens) * 10
		},
		Clock:   f.clock,
		IDs:     f.ids,
		Content: reg,
		Effects: dispatchRecorder{got: &got},
	})
	if err != nil {
		t.Fatalf("NewStepper: %v", err)
	}
	// noteEffects fails its build for the "fail" payload.
	f.prov.ReplyText("```workspace:note\n{\"text\":\"fail\"}\n```\n")

	if _, err := s.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"}); err == nil {
		t.Fatal("a failing effect must fail the step")
	}
	committed := f.hist.committed()
	if len(committed) != 1 || committed[0].Actor.Kind != domain.ActorAgent {
		t.Fatalf("the assistant turn must stay committed, got %v turns", len(committed))
	}
}

func TestStepCommitsToolResultsNamingTheirCall(t *testing.T) {
	f := newStepFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "search"}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call42", Name: "search", Arguments: json.RawMessage(`{}`)})

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(res.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(res.Turns))
	}
	turn := res.Turns[1]
	if turn.Actor.Kind != domain.ActorTool {
		t.Errorf("actor = %v, want tool", turn.Actor.Kind)
	}
	if turn.Parent == nil || turn.Parent.ID != res.Turns[0].ID {
		t.Errorf("the result turn must be a child of the assistant turn, got %v", turn.Parent)
	}
	if len(turn.Blocks) != 1 {
		t.Fatalf("blocks = %+v", turn.Blocks)
	}
	value, _ := turn.Blocks[0].Value.(map[string]any)
	if value["call_id"] != "call42" {
		t.Errorf("call_id = %v, want call42", value["call_id"])
	}
	if value["is_error"] == true {
		t.Error("a successful tool must not be flagged as an error")
	}
}

func TestStepNarrowsTheToolPrincipal(t *testing.T) {
	f := newStepFixture(t)
	f.register(t,
		countingTool(f.rec, agent.ToolSpec{Name: "guarded", Scopes: []string{"fs:read"}}),
		countingTool(f.rec, agent.ToolSpec{Name: "open"}),
	)
	f.prov.ReplyToolCalls(
		provider.ToolCallPart{CallID: "c1", Name: "guarded", Arguments: json.RawMessage(`{}`)},
		provider.ToolCallPart{CallID: "c2", Name: "open", Arguments: json.RawMessage(`{}`)},
	)

	_, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()

	guarded := f.rec.principals["guarded"]
	if guarded.Kind != kernel.AgentPrincipal {
		t.Errorf("kind = %v, want an agent principal", guarded.Kind)
	}
	if guarded.Owner != "u1" || guarded.Subject != "u1" {
		t.Errorf("identity changed: %+v", guarded)
	}
	// The user held fs:read and other:scope; only fs:read is required by an
	// exposed tool, so the other scope is dropped and none is added.
	if !slices.Equal(guarded.Scopes, []string{"fs:read"}) {
		t.Errorf("narrowed scopes = %v, want [fs:read]", guarded.Scopes)
	}
	// The principal is narrowed once for the whole step, not per tool, so a
	// tool that needs nothing still runs as the run's identity.
	open := f.rec.principals["open"]
	if !slices.Equal(open.Scopes, guarded.Scopes) {
		t.Errorf("open ran under %v, want the same narrowed identity %v", open.Scopes, guarded.Scopes)
	}
	if open.Kind != kernel.AgentPrincipal {
		t.Errorf("open ran as %v, want an agent principal", open.Kind)
	}
}

func TestStepExposesOnlyTheRequestedTools(t *testing.T) {
	f := newStepFixture(t)
	f.register(t,
		countingTool(f.rec, agent.ToolSpec{Name: "alpha"}),
		countingTool(f.rec, agent.ToolSpec{Name: "beta"}),
	)
	f.prov.ReplyText("ok")

	if _, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{
		Model: "m", Tools: []string{"beta"},
	}); err != nil {
		t.Fatalf("Step: %v", err)
	}
	requests := f.prov.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "beta" {
		t.Fatalf("tools exposed = %+v, want [beta]", requests[0].Tools)
	}
}

func TestStepRunsToolsInParallelAndKeepsResultsOrdered(t *testing.T) {
	f := newStepFixture(t)
	f.rec.arrived = make(chan struct{}, 4)
	f.rec.release = make(chan struct{})

	const n = 8
	calls := make([]provider.ToolCallPart, 0, n)
	for i := range n {
		name := fmt.Sprintf("t%d", i)
		f.register(t, barrierTool(f.rec, agent.ToolSpec{Name: name}))
		calls = append(calls, provider.ToolCallPart{
			CallID: fmt.Sprintf("call%d", i), Name: name, Arguments: json.RawMessage(`{}`),
		})
	}
	// Release the first four only once all four have arrived: if the tools
	// did not run in parallel, this would deadlock and the test would time
	// out rather than pass by luck.
	go func() {
		for range 4 {
			<-f.rec.arrived
		}
		close(f.rec.release)
	}()
	f.prov.ReplyToolCalls(calls...)

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m", MaxParallel: 4})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(res.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(res.Turns))
	}
	blocks := res.Turns[1].Blocks
	if len(blocks) != n {
		t.Fatalf("result blocks = %d, want %d", len(blocks), n)
	}
	for i, blk := range blocks {
		value, _ := blk.Value.(map[string]any)
		if value["call_id"] != fmt.Sprintf("call%d", i) {
			t.Errorf("block %d answers %v, want call%d", i, value["call_id"], i)
		}
	}
}

func TestStepPendsRiskConfirmToolsWithoutRunningThem(t *testing.T) {
	f := newStepFixture(t)
	f.register(t,
		countingTool(f.rec, agent.ToolSpec{Name: "deletes", Risk: agent.RiskConfirm}),
		countingTool(f.rec, agent.ToolSpec{Name: "search"}),
	)
	f.prov.ReplyToolCalls(
		provider.ToolCallPart{CallID: "c1", Name: "deletes", Arguments: json.RawMessage(`{}`)},
		provider.ToolCallPart{CallID: "c2", Name: "search", Arguments: json.RawMessage(`{}`)},
	)

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "deletes" {
		t.Fatalf("pending = %+v, want just deletes", res.Pending)
	}
	for _, ev := range f.rec.all() {
		if ev == "tool:deletes" {
			t.Fatal("a agent.RiskConfirm tool must not run before approval")
		}
	}
}

func TestStepLeavesNoResultTurnWhenEverythingIsPending(t *testing.T) {
	f := newStepFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "deletes", Risk: agent.RiskConfirm}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "deletes", Arguments: json.RawMessage(`{}`)})

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(res.Turns) != 1 {
		t.Errorf("turns = %d, want only the assistant turn", len(res.Turns))
	}
}

func TestStepTurnsAnUnknownToolIntoAnErrorResult(t *testing.T) {
	f := newStepFixture(t)
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "ghost", Arguments: json.RawMessage(`{}`)})

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(res.Turns) != 2 {
		t.Fatalf("turns = %d, want the assistant turn and the results", len(res.Turns))
	}
	value, _ := res.Turns[1].Blocks[0].Value.(map[string]any)
	if value["is_error"] != true {
		t.Error("an unknown tool must be an error result")
	}
	if !strings.Contains(fmt.Sprint(value["output"]), "unknown tool") {
		t.Errorf("the model should be told why: %v", value["output"])
	}
}

func TestStepTurnsAToolFailureIntoAnErrorResult(t *testing.T) {
	f := newStepFixture(t)
	f.register(t, failingTool(f.rec, agent.ToolSpec{Name: "flaky"}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "flaky", Arguments: json.RawMessage(`{"arg":"now"}`)})

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("a tool failure must not fail the step: %v", err)
	}
	value, _ := res.Turns[1].Blocks[0].Value.(map[string]any)
	if value["is_error"] != true {
		t.Fatal("a tool failure must reach the model as an error result")
	}
	if !strings.Contains(fmt.Sprint(value["output"]), "boom: now") {
		t.Errorf("output = %v", value["output"])
	}
}

func TestStepDiscardsUncommittedWorkOnCancellation(t *testing.T) {
	f := newStepFixture(t)
	f.prov.ReplyText("never committed")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.stepper.Step(ctx, testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if !kernel.IsCode(err, kernel.CodeCanceled) {
		t.Fatalf("err = %v, want CANCELED", err)
	}
	if len(f.hist.committed()) != 0 {
		t.Fatalf("a cancelled step committed %d turns", len(f.hist.committed()))
	}
}

func TestStepRejectsArgumentsThatFailTheToolSchema(t *testing.T) {
	f := newStepFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "search"}))
	// argTool's field is required here: the explicit, non-omitempty shape.
	tool := agent.NewTool(agent.ToolSpec{Name: "needs"}, func(ctx context.Context, p kernel.Principal, in struct {
		Arg string `json:"arg"`
	}) (string, error) {
		return "", nil
	})
	f.register(t, tool)
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "needs", Arguments: json.RawMessage(`{}`)})

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(res.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(res.Turns))
	}
	value, _ := res.Turns[1].Blocks[0].Value.(map[string]any)
	if value["is_error"] != true {
		t.Fatalf("a schema-invalid call must reach the model as an error, got %v", value)
	}
	if !strings.Contains(fmt.Sprint(value["output"]), "schema") {
		t.Errorf("output = %v, want a schema complaint", value["output"])
	}
	for _, ev := range f.rec.all() {
		if strings.HasPrefix(ev, "tool:") {
			t.Errorf("the tool must not run when its arguments fail the schema, got %s", ev)
		}
	}
}

func TestStepDiscardsToolResultsOnCancellation(t *testing.T) {
	f := newStepFixture(t)

	// A tool that cancels the step: by the time it runs, the assistant turn
	// is already committed, which is exactly the window the rule covers.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sawCancelled := false
	f.register(t, agent.NewTool(agent.ToolSpec{Name: "cancels"},
		func(ctx context.Context, p kernel.Principal, in argTool) (map[string]string, error) {
			f.rec.add("tool:cancels")
			cancel()
			return nil, nil
		}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "cancels", Arguments: json.RawMessage(`{"arg":"x"}`)})

	_, stepErr := f.stepper.Step(ctx, testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if !kernel.IsCode(stepErr, kernel.CodeCanceled) {
		t.Fatalf("err = %v, want CANCELED", stepErr)
	}
	sawCancelled = ctx.Err() != nil
	if !sawCancelled {
		t.Fatal("the cancelling tool never ran")
	}

	committed := f.hist.committed()
	if len(committed) != 1 {
		t.Fatalf("committed %d turns, want just the assistant turn", len(committed))
	}
	if committed[0].Actor.Kind != domain.ActorAgent {
		t.Errorf("the assistant turn must be the one that survived, got %v", committed[0].Actor.Kind)
	}
	// And the timeline proves the assistant turn was committed before the
	// tool ran, so the calls can be re-run after the restart.
	events := f.line.all()
	want := []string{"read:c1", "append:agent", "tool:cancels"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestStepRejectsMissingConversationAndModel(t *testing.T) {
	f := newStepFixture(t)
	if _, err := f.stepper.Step(context.Background(), testPrincipal(), "", agent.StepOptions{Model: "m"}); !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Errorf("empty conversation: err = %v, want INVALID", err)
	}
	if _, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{}); !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Errorf("empty model: err = %v, want INVALID", err)
	}
}

func TestStepRecordsUsageAndCost(t *testing.T) {
	f := newStepFixture(t)
	f.prov.ReplyText("done")

	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	// The scripted provider reports no usage, so both are zero; the point is
	// the pricer ran without error and the values survived the commit.
	if res.CostMicros != 0 {
		t.Errorf("cost = %d, want 0 for a usage-less reply", res.CostMicros)
	}
	if res.Usage.InputTokens != 0 || res.Usage.OutputTokens != 0 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Turns[0].Usage.Model != "m" {
		t.Errorf("committed usage model = %q, want m", res.Turns[0].Usage.Model)
	}
}

func TestStepSurfacesHistoryFailures(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		f := newStepFixture(t)
		f.prov.ReplyText("never read")
		f.hist.readErr = kernel.NewError(kernel.CodeUnavailable, "history is down")
		_, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
		if !kernel.IsCode(err, kernel.CodeUnavailable) {
			t.Errorf("err = %v, want UNAVAILABLE", err)
		}
		if len(f.hist.committed()) != 0 {
			t.Error("nothing must be committed when the snapshot cannot be read")
		}
	})

	t.Run("assistant turn", func(t *testing.T) {
		f := newStepFixture(t)
		f.prov.ReplyText("never committed")
		f.hist.appendErr = kernel.NewError(kernel.CodeBusy, "append refused")
		_, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
		if !kernel.IsCode(err, kernel.CodeBusy) {
			t.Errorf("err = %v, want BUSY", err)
		}
	})

	t.Run("tool results", func(t *testing.T) {
		f := newStepFixture(t)
		f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "search"}))
		f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "search", Arguments: json.RawMessage(`{}`)})
		// The assistant turn commits first; only the results are refused.
		f.hist.failAfter(1)
		_, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
		if err == nil {
			t.Fatal("a refused tool-result append must surface as an error")
		}
	})
}

func TestStepSurfacesAProviderFailure(t *testing.T) {
	f := newStepFixture(t)
	// No reply queued: the scripted provider fails the call.
	_, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err == nil {
		t.Fatal("a provider failure must reach the caller")
	}
	if len(f.hist.committed()) != 0 {
		t.Error("nothing must be committed when the provider fails")
	}
}

func TestStepWithoutAPricerChargesNothing(t *testing.T) {
	f := newStepFixtureWith(t, nil)
	f.prov.ReplyText("free")
	res, err := f.stepper.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if res.CostMicros != 0 {
		t.Errorf("cost = %d, want 0 without a pricer", res.CostMicros)
	}
}

func TestResolveValidatesTheCallAndHonorsCancellation(t *testing.T) {
	f := newStepFixture(t)
	resolver := f.stepper.(agent.CallResolver)
	parent := domain.TurnRef{ID: "t1", Version: 1}

	_, err := resolver.Resolve(context.Background(), testPrincipal(), "c1", parent,
		agent.ToolCall{}, true, "")
	if !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Errorf("an empty call err = %v, want INVALID", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = resolver.Resolve(ctx, testPrincipal(), "c1", parent,
		agent.ToolCall{CallID: "c1", Name: "search"}, true, "")
	if !kernel.IsCode(err, kernel.CodeCanceled) {
		t.Errorf("a cancelled resolution err = %v, want CANCELED", err)
	}
	if len(f.hist.committed()) != 0 {
		t.Error("a cancelled resolution must not commit")
	}
}
