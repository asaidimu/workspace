package agent_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/memory"
)

// recordingEmitter captures what the manager announced, in order.
type recordingEmitter struct {
	mu     sync.Mutex
	events []kernel.Event
	owners []string
}

func (e *recordingEmitter) Emit(ev kernel.Event, owner, conversationID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
	e.owners = append(e.owners, owner)
}

func (e *recordingEmitter) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.events))
	for _, ev := range e.events {
		out = append(out, ev.EventName())
	}
	return out
}

func (e *recordingEmitter) has(name string) bool {
	for _, n := range e.names() {
		if n == name {
			return true
		}
	}
	return false
}

type managerFixture struct {
	*stepFixture
	store    store.Store
	emitter  *recordingEmitter
	manager  agent.RunManager
	resolver agent.CallResolver
	scope    store.Scope
}

func newManagerFixture(t *testing.T) *managerFixture {
	t.Helper()
	return newManagerFixtureFrom(t, newStepFixture(t))
}

// newManagerFixtureFrom builds the manager over an already-constructed step
// fixture, so a test can choose the stepper's pricer or history.
func newManagerFixtureFrom(t *testing.T, f *stepFixture) *managerFixture {
	t.Helper()
	resolver, ok := f.stepper.(agent.CallResolver)
	if !ok {
		t.Fatal("the step primitive must resolve pending calls")
	}
	st := memory.New(memory.Options{Clock: f.clock})
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	emitter := &recordingEmitter{}
	mgr, err := agent.NewManager(agent.ManagerConfig{
		Store:    st,
		Stepper:  f.stepper,
		Resolver: resolver,
		Emitter:  emitter,
		Clock:    f.clock,
		IDs:      f.ids,
		Worker:   "test-worker",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return &managerFixture{
		stepFixture: f,
		store:       st,
		emitter:     emitter,
		manager:     mgr,
		resolver:    resolver,
		scope:       store.Scope{Owner: "u1"},
	}
}

func (f *managerFixture) repos(t *testing.T) store.Repos {
	t.Helper()
	repos, err := f.store.Scoped(f.scope)
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	return repos
}

func (f *managerFixture) run(t *testing.T, id string) domain.Run {
	t.Helper()
	run, err := f.repos(t).Runs().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return run
}

// waitStatus spins on a condition rather than sleeping: the run's state is
// the only thing that decides when a test can continue.
func waitStatus(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func (f *managerFixture) waitRunStatus(t *testing.T, id string, want domain.RunStatus) domain.Run {
	t.Helper()
	var got domain.Run
	waitStatus(t, "run "+id+" to be "+string(want), func() bool {
		run, err := f.repos(t).Runs().Get(context.Background(), id)
		if err != nil {
			return false
		}
		got = run
		return run.Status == want
	})
	return got
}

func (f *managerFixture) waitEvent(t *testing.T, name string) {
	t.Helper()
	waitStatus(t, "event "+name, func() bool { return f.emitter.has(name) })
}

func TestNewManagerRejectsIncompleteConfig(t *testing.T) {
	f := newStepFixture(t)
	base := func(c *agent.ManagerConfig) {
		c.Store = memory.New(memory.Options{})
		c.Stepper = f.stepper
		c.Resolver = f.stepper.(agent.CallResolver)
		c.Clock = f.clock
		c.IDs = f.ids
	}
	cases := []struct {
		name string
		mut  func(*agent.ManagerConfig)
	}{
		{"no store", func(c *agent.ManagerConfig) { c.Store = nil }},
		{"no stepper", func(c *agent.ManagerConfig) { c.Stepper = nil }},
		{"no resolver", func(c *agent.ManagerConfig) { c.Resolver = nil }},
		{"no clock", func(c *agent.ManagerConfig) { c.Clock = nil }},
		{"no ids", func(c *agent.ManagerConfig) { c.IDs = nil }},
	}
	for _, c := range cases {
		cfg := agent.ManagerConfig{}
		base(&cfg)
		c.mut(&cfg)
		if _, err := agent.NewManager(cfg); !kernel.IsCode(err, kernel.CodeInvalid) {
			t.Errorf("%s: err = %v, want INVALID", c.name, err)
		}
	}
}

func TestStartDrivesTheRunToSuccess(t *testing.T) {
	f := newManagerFixture(t)
	f.prov.ReplyText("done")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.Status != domain.RunQueued {
		t.Errorf("Start returned status %q, want queued", run.Status)
	}
	f.waitEvent(t, "run.started")
	final := f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	if final.Failure != "" {
		t.Errorf("failure = %q", final.Failure)
	}
}

func TestStartAppliesDefaultLimits(t *testing.T) {
	f := newManagerFixture(t)
	f.prov.ReplyText("done")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunSucceeded)

	var st struct {
		Options agent.RunOptions `json:"options"`
	}
	got := f.run(t, run.ID)
	if err := json.Unmarshal(got.State, &st); err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Options.MaxSteps != agent.DefaultMaxSteps {
		t.Errorf("MaxSteps = %d, want %d", st.Options.MaxSteps, agent.DefaultMaxSteps)
	}
	if st.Options.Deadline != agent.DefaultDeadline {
		t.Errorf("Deadline = %v, want %v", st.Options.Deadline, agent.DefaultDeadline)
	}
	if st.Options.MaxParallel != agent.DefaultMaxParallel {
		t.Errorf("MaxParallel = %d, want %d", st.Options.MaxParallel, agent.DefaultMaxParallel)
	}
}

func TestStepBudgetExhaustsTheRun(t *testing.T) {
	f := newManagerFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "search"}))
	// Two replies that both ask for tools: the first step spends the whole
	// budget of one, so the loop must fail rather than start a second.
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "search", Arguments: json.RawMessage(`{}`)})
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c2", Name: "search", Arguments: json.RawMessage(`{}`)})

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m", MaxSteps: 1})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := f.waitRunStatus(t, run.ID, domain.RunFailed)
	if !strings.Contains(final.Failure, "step budget") {
		t.Errorf("failure = %q, want the step budget exhausted", final.Failure)
	}
}

func TestCostCapFailsTheRun(t *testing.T) {
	// A pricer that charges on every call, so the first committed step alone
	// overshoots the cap. The provider reports no usage; the cap is about
	// what the run spends, not about the vendor's numbers.
	f := newStepFixtureWith(t, func(m string, u provider.Usage) int64 { return 1_000 })
	f.prov.ReplyText("priced")
	mf := newManagerFixtureFrom(t, f)

	run, err := mf.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{
		Model: "m", CostCapMicros: 10,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := mf.waitRunStatus(t, run.ID, domain.RunFailed)
	if !strings.Contains(final.Failure, "cost cap") {
		t.Errorf("failure = %q, want the cost cap", final.Failure)
	}
}

func TestDeadlineFailsTheRun(t *testing.T) {
	f := newManagerFixture(t)

	// A tool that moves the injected clock past the run's deadline, so the
	// loop's own check trips it without any real time passing.
	f.register(t, agent.NewTool(agent.ToolSpec{Name: "slow"},
		func(ctx context.Context, p kernel.Principal, in argTool) (map[string]string, error) {
			f.clock.Advance(2 * time.Minute)
			return map[string]string{"ok": "true"}, nil
		}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "slow", Arguments: json.RawMessage(`{}`)})

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{
		Model: "m", Deadline: time.Minute,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := f.waitRunStatus(t, run.ID, domain.RunFailed)
	if !strings.Contains(final.Failure, "deadline") {
		t.Errorf("failure = %q, want the deadline", final.Failure)
	}
}

func TestCancelStopsARunItOwns(t *testing.T) {
	f := newManagerFixture(t)

	started := make(chan struct{})
	f.register(t, agent.NewTool(agent.ToolSpec{Name: "blocks"},
		func(ctx context.Context, p kernel.Principal, in argTool) (map[string]string, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "blocks", Arguments: json.RawMessage(`{}`)})

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitStatus(t, "the blocking tool to start", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	// The caller's own context is untouched by the cancellation.
	callerCtx, callerCancel := context.WithCancel(context.Background())
	defer callerCancel()
	if err := f.manager.Cancel(callerCtx, testPrincipal(), run.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if callerCtx.Err() != nil {
		t.Error("the caller's context must not be cancelled by run.cancel")
	}
	f.waitRunStatus(t, run.ID, domain.RunCancelled)
	f.waitEvent(t, "run.cancelled")
}

func TestCancelRejectsForeignAndFinishedRuns(t *testing.T) {
	f := newManagerFixture(t)
	f.prov.ReplyText("done")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunSucceeded)

	foreign := kernel.Principal{Owner: "someone-else", Subject: "someone-else", Kind: kernel.UserPrincipal}
	if err := f.manager.Cancel(context.Background(), foreign, run.ID); !kernel.IsCode(err, kernel.CodeForbidden) {
		t.Errorf("foreign cancel err = %v, want FORBIDDEN", err)
	}
	if err := f.manager.Cancel(context.Background(), testPrincipal(), run.ID); !kernel.IsCode(err, kernel.CodeConflict) {
		t.Errorf("cancelling a finished run err = %v, want CONFLICT", err)
	}
	if err := f.manager.Cancel(context.Background(), testPrincipal(), "nope"); !kernel.IsCode(err, kernel.CodeNotFound) {
		t.Errorf("cancelling a missing run err = %v, want NOT_FOUND", err)
	}
}

func TestApprovalParksTheRunAndApproveResumesIt(t *testing.T) {
	f := newManagerFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "deletes", Risk: agent.RiskConfirm}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call-1", Name: "deletes", Arguments: json.RawMessage(`{}`)})
	f.prov.ReplyText("after approval")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.waitEvent(t, "run.approval_requested")
	parked := f.waitRunStatus(t, run.ID, domain.RunWaiting)
	if parked.Step != 1 {
		t.Errorf("step = %d, want the assistant step committed", parked.Step)
	}
	for _, ev := range f.rec.all() {
		if ev == "tool:deletes" {
			t.Fatal("a RiskConfirm tool ran before approval")
		}
	}

	if err := f.manager.Approve(context.Background(), testPrincipal(), run.ID, "call-1", true, "looks fine"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	f.waitEvent(t, "run.resumed")
	final := f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	if final.Failure != "" {
		t.Errorf("failure = %q", final.Failure)
	}
	if !contains(f.rec.all(), "tool:deletes") {
		t.Error("the approved tool never ran")
	}
}

func TestRejectCommitsAnErrorResult(t *testing.T) {
	f := newManagerFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "deletes", Risk: agent.RiskConfirm}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call-1", Name: "deletes", Arguments: json.RawMessage(`{}`)})
	f.prov.ReplyText("after rejection")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunWaiting)

	if err := f.manager.Approve(context.Background(), testPrincipal(), run.ID, "call-1", false, "too dangerous"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunSucceeded)

	for _, ev := range f.rec.all() {
		if ev == "tool:deletes" {
			t.Fatal("a rejected tool must not run")
		}
	}
	// The committed result turn tells the model why.
	turns := f.hist.committed()
	var sawErrorResult bool
	for _, turn := range turns {
		for _, blk := range turn.Blocks {
			if blk.Type != "tool_result" {
				continue
			}
			value, _ := blk.Value.(map[string]any)
			if value["is_error"] == true && strings.Contains(fmtValue(value["output"]), "too dangerous") {
				sawErrorResult = true
			}
		}
	}
	if !sawErrorResult {
		t.Errorf("no tool_result carried the rejection reason; turns = %+v", turns)
	}
}

func TestApproveRequiresAParkedRun(t *testing.T) {
	f := newManagerFixture(t)
	f.prov.ReplyText("done")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunSucceeded)

	if err := f.manager.Approve(context.Background(), testPrincipal(), run.ID, "call-1", true, ""); !kernel.IsCode(err, kernel.CodeConflict) {
		t.Errorf("approving a finished run err = %v, want CONFLICT", err)
	}
}

func TestApproveRejectsAMissingCallAndAForeignOwner(t *testing.T) {
	f := newManagerFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "deletes", Risk: agent.RiskConfirm}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call-1", Name: "deletes", Arguments: json.RawMessage(`{}`)})

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunWaiting)

	if err := f.manager.Approve(context.Background(), testPrincipal(), run.ID, "call-404", true, ""); !kernel.IsCode(err, kernel.CodeNotFound) {
		t.Errorf("missing call err = %v, want NOT_FOUND", err)
	}
	foreign := kernel.Principal{Owner: "someone-else", Subject: "someone-else", Kind: kernel.UserPrincipal}
	if err := f.manager.Approve(context.Background(), foreign, run.ID, "call-1", true, ""); !kernel.IsCode(err, kernel.CodeForbidden) {
		t.Errorf("foreign approve err = %v, want FORBIDDEN", err)
	}
}

func TestRecoverClaimsStaleRunsAndLeavesFreshOnes(t *testing.T) {
	f := newManagerFixture(t)
	f.prov.ReplyText("recovered")

	// A run another process died holding: running, stale heartbeat, state
	// encoded exactly as the manager encodes it.
	staleState, err := agent.EncodeRunState(testPrincipal(), agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("EncodeRunState: %v", err)
	}
	repos := f.repos(t)
	// Create stamps HeartbeatAt with the store's now, so the stale run has to
	// be created before the clock moves and the fresh one after it.
	if _, err := repos.Runs().Create(context.Background(), domain.Run{
		ID: "stale", ConversationID: "c1", Status: domain.RunRunning,
		State: staleState, Worker: "dead-worker",
	}); err != nil {
		t.Fatalf("Create stale: %v", err)
	}
	f.clock.Advance(time.Hour)
	if _, err := repos.Runs().Create(context.Background(), domain.Run{
		ID: "fresh", ConversationID: "c1", Status: domain.RunRunning,
		State: staleState, Worker: "other-worker",
	}); err != nil {
		t.Fatalf("Create fresh: %v", err)
	}

	if err := f.manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	f.waitRunStatus(t, "stale", domain.RunSucceeded)

	fresh := f.run(t, "fresh")
	if fresh.Status != domain.RunRunning {
		t.Errorf("a fresh run must be left alone, got %q", fresh.Status)
	}
	if fresh.Worker != "other-worker" {
		t.Errorf("a fresh run's worker changed to %q", fresh.Worker)
	}
}

func TestManagerIsAStepperAndACheckpointer(t *testing.T) {
	f := newManagerFixture(t)
	f.prov.ReplyText("hello")

	var _ agent.RunManager = f.manager

	res, err := f.manager.Step(context.Background(), testPrincipal(), "c1", agent.StepOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Step through the manager: %v", err)
	}
	if len(res.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(res.Turns))
	}

	_, err = f.manager.Checkpoint(context.Background(), testPrincipal(), "nope", 0, nil)
	if !kernel.IsCode(err, kernel.CodeNotFound) {
		t.Errorf("checkpointing a missing run err = %v, want NOT_FOUND", err)
	}
}

func TestCheckpointRefusesAForeignOwner(t *testing.T) {
	f := newManagerFixture(t)
	staleState, err := agent.EncodeRunState(testPrincipal(), agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("EncodeRunState: %v", err)
	}
	if _, err := f.repos(t).Runs().Create(context.Background(), domain.Run{
		ID: "r1", ConversationID: "c1", Status: domain.RunRunning, State: staleState,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	foreign := kernel.Principal{Owner: "someone-else", Subject: "someone-else", Kind: kernel.UserPrincipal}
	if _, err := f.manager.Checkpoint(context.Background(), foreign, "r1", 0, nil); !kernel.IsCode(err, kernel.CodeForbidden) {
		t.Errorf("foreign checkpoint err = %v, want FORBIDDEN", err)
	}
}

// contains reports whether the list holds the element.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// fmtValue renders a tool result's output, which may already be JSON bytes.
func fmtValue(v any) string {
	if b, ok := v.(json.RawMessage); ok {
		return string(b)
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestHeartbeatKeepsAWorkerlessRunAlive(t *testing.T) {
	f := newManagerFixture(t)
	// A one-millisecond heartbeat so the ticker fires inside a running step;
	// the test waits on the stored heartbeat rather than on time passing.
	st := memory.New(memory.Options{Clock: f.clock})
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	mgr, err := agent.NewManager(agent.ManagerConfig{
		Store:     st,
		Stepper:   f.stepper,
		Resolver:  f.stepper.(agent.CallResolver),
		Clock:     f.clock,
		IDs:       f.ids,
		Heartbeat: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	started := make(chan struct{})
	f.register(t, agent.NewTool(agent.ToolSpec{Name: "holds"},
		func(ctx context.Context, p kernel.Principal, in argTool) (map[string]string, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "c1", Name: "holds", Arguments: json.RawMessage(`{}`)})

	run, err := mgr.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitStatus(t, "the holding tool to start", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	repos, err := st.Scoped(store.Scope{Owner: "u1"})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	// The store stamps the heartbeat with the injected clock, which does not
	// move on its own: advance it on every spin so a heartbeat written with
	// it is observably later than the one Create wrote.
	waitStatus(t, "the run's heartbeat to advance", func() bool {
		f.clock.Advance(time.Millisecond)
		got, err := repos.Runs().Get(context.Background(), run.ID)
		return err == nil && got.HeartbeatAt.After(run.HeartbeatAt)
	})
	if err := mgr.Cancel(context.Background(), testPrincipal(), run.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// This manager writes to st, not to the fixture's store, so the
	// assertion follows it there.
	waitStatus(t, "the run to be cancelled", func() bool {
		got, err := repos.Runs().Get(context.Background(), run.ID)
		return err == nil && got.Status == domain.RunCancelled
	})
}

func TestCancellingARunThisProcessDoesNotDrive(t *testing.T) {
	f := newManagerFixture(t)
	staleState, err := agent.EncodeRunState(testPrincipal(), agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("EncodeRunState: %v", err)
	}
	if _, err := f.repos(t).Runs().Create(context.Background(), domain.Run{
		ID: "elsewhere", ConversationID: "c1", Status: domain.RunRunning, State: staleState,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A restart's view: this process holds no context for the run, so
	// stopDriving finds nothing and the state is committed directly.
	if err := f.manager.Cancel(context.Background(), testPrincipal(), "elsewhere"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := f.run(t, "elsewhere"); got.Status != domain.RunCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	f.waitEvent(t, "run.cancelled")
}

func TestRecoverFailsARunWhoseStateIsCorrupt(t *testing.T) {
	f := newManagerFixture(t)
	if _, err := f.repos(t).Runs().Create(context.Background(), domain.Run{
		ID: "broken", ConversationID: "c1", Status: domain.RunRunning,
		State: json.RawMessage("{not json"),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create stamps HeartbeatAt with the store's now, so staleness only comes
	// from moving the injected clock past it.
	f.clock.Advance(time.Hour)
	if err := f.manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	final := f.waitRunStatus(t, "broken", domain.RunFailed)
	if final.Failure == "" {
		t.Error("a corrupt run must say why it failed")
	}
}

func TestCheckpointWritesStateAndAdvancesTheStep(t *testing.T) {
	f := newManagerFixture(t)
	staleState, err := agent.EncodeRunState(testPrincipal(), agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("EncodeRunState: %v", err)
	}
	if _, err := f.repos(t).Runs().Create(context.Background(), domain.Run{
		ID: "r1", ConversationID: "c1", Status: domain.RunRunning, State: staleState,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	payload := json.RawMessage(`{"loop":"research"}`)
	got, err := f.manager.Checkpoint(context.Background(), testPrincipal(), "r1", 0, payload)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got.Step != 1 {
		t.Errorf("step = %d, want 1", got.Step)
	}
	if string(got.State) != string(payload) {
		t.Errorf("state = %s, want %s", got.State, payload)
	}
	// A stale step is a conflict, the same CAS the port enforces.
	if _, err := f.manager.Checkpoint(context.Background(), testPrincipal(), "r1", 0, nil); !kernel.IsCode(err, kernel.CodeConflict) {
		t.Errorf("stale checkpoint err = %v, want CONFLICT", err)
	}
}

func TestStartRejectsBadInput(t *testing.T) {
	f := newManagerFixture(t)
	if _, err := f.manager.Start(context.Background(), testPrincipal(), "", agent.RunOptions{Model: "m"}); !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Errorf("empty conversation err = %v, want INVALID", err)
	}
	noOwner := kernel.Principal{Subject: "u1", Kind: kernel.UserPrincipal}
	if _, err := f.manager.Start(context.Background(), noOwner, "c1", agent.RunOptions{Model: "m"}); !kernel.IsCode(err, kernel.CodeForbidden) {
		t.Errorf("ownerless start err = %v, want FORBIDDEN", err)
	}
	if err := f.manager.Cancel(context.Background(), noOwner, "r1"); !kernel.IsCode(err, kernel.CodeForbidden) {
		t.Errorf("ownerless cancel err = %v, want FORBIDDEN", err)
	}
}

func TestEncodeRunStateRequiresAnOwner(t *testing.T) {
	if _, err := agent.EncodeRunState(kernel.Principal{}, agent.RunOptions{Model: "m"}); !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Errorf("err = %v, want INVALID", err)
	}
}

func TestApproveRefusesCorruptRunState(t *testing.T) {
	f := newManagerFixture(t)
	if _, err := f.repos(t).Runs().Create(context.Background(), domain.Run{
		ID: "rotten", ConversationID: "c1", Status: domain.RunWaiting,
		State: json.RawMessage("{not json"),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := f.manager.Approve(context.Background(), testPrincipal(), "rotten", "c", true, "")
	if !kernel.IsCode(err, kernel.CodeInternal) {
		t.Errorf("err = %v, want INTERNAL", err)
	}
}

// approvalWindowLapse is just past section 11's 24-hour approval window.
const approvalWindowLapse = 25 * time.Hour

func TestApprovalWindowLapseBecomesARejection(t *testing.T) {
	f := newManagerFixture(t)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "deletes", Risk: agent.RiskConfirm}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call-1", Name: "deletes", Arguments: json.RawMessage(`{}`)})
	f.prov.ReplyText("after the lapse")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunWaiting)

	// Nobody answers for a day: a late approval must behave like a
	// rejection rather than run the tool, so the model is told no.
	f.clock.Advance(approvalWindowLapse)
	if err := f.manager.Approve(context.Background(), testPrincipal(), run.ID, "call-1", true, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	for _, ev := range f.rec.all() {
		if ev == "tool:deletes" {
			t.Fatal("a call whose approval window lapsed must not run")
		}
	}
	var sawLapse bool
	for _, turn := range f.hist.committed() {
		for _, blk := range turn.Blocks {
			if blk.Type != "tool_result" {
				continue
			}
			value, _ := blk.Value.(map[string]any)
			if value["is_error"] == true && strings.Contains(fmtValue(value["output"]), "lapsed") {
				sawLapse = true
			}
		}
	}
	if !sawLapse {
		t.Error("the lapse must reach the model as an error result")
	}
}
