package agent_test

// Failure-injection tests for the run loop's checkpoints (Workspace
// specification, section 11, and the Phase 4 gate: "failure-injection tests
// pass at every checkpoint"). The drive loop persists at four points:
//
//  1. SetStatus(running) when it claims a queued run,
//  2. one Checkpoint (CAS on the step counter) after every step,
//  3. SetStatus(waiting) when a RiskConfirm call parks the run,
//  4. Finish for a terminal state.
//
// Two fault modes cover the two ways a store can misbehave:
//
//   - transient: the first call to the target method fails and then heals.
//     The loop reports through fail() and the run ends failed — except at
//     Finish, whose error is swallowed and whose run the loop's redrive
//     finishes on the next pass. A store error is a failure, not a crash.
//
//   - crash: the call blocks until the test releases it and never reaches
//     the store — a process dying mid-write. Its heartbeats stop with it, so
//     advancing the injected clock makes the run stale. Recovery comes from
//     a second manager over the healthy store: the crashed manager still
//     holds its driving flag for the run, so its own Recover could never
//     re-drive it.
//
// Each test asserts the invariant: no committed turn is lost, at most the
// step in flight is repeated, and the run reaches a sane state.

import (
	"context"
	"encoding/json"
	"errors"
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

// faultMode selects how a store misbehaves.
type faultMode int

const (
	// modeError fails the first matching call with an error and then heals:
	// a store that hiccuped, not a process that died.
	modeError faultMode = iota
	// modeCrash blocks the first matching call until the test releases it
	// and never hands the write to the store. The run's heartbeats stop
	// with the dead worker, so the run goes stale.
	modeCrash
)

// errInjected is the failure every injected checkpoint produces.
var errInjected = errors.New("injected store failure")

// faultStore wraps a store.Store and injects a failure at one of the run
// loop's checkpoints: Checkpoint, SetStatus(running), SetStatus(waiting) or
// Finish. It fires once on the first matching call; every later call passes
// through, so recovery runs against a healthy store.
type faultStore struct {
	inner  store.Store
	target string
	mode   faultMode

	mu      sync.Mutex
	fired   bool
	hit     chan struct{} // closed the moment the fault fires
	release chan struct{} // crash mode: closing it lets the blocked call fail
}

func newFaultStore(inner store.Store, target string, mode faultMode) *faultStore {
	return &faultStore{
		inner:   inner,
		target:  target,
		mode:    mode,
		hit:     make(chan struct{}),
		release: make(chan struct{}),
	}
}

// intercept decides what a call to method must do. block tells a crash-mode
// caller to wait for release before returning the error; the write must
// never reach the store, because the process that meant to make it is gone.
func (s *faultStore) intercept(method string) (fail, block bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fired || method != s.target {
		return false, false
	}
	s.fired = true
	close(s.hit)
	return true, s.mode == modeCrash
}

// crashed reports whether the fault has fired in crash mode: from that
// moment this store's worker is dead and sends no heartbeats.
func (s *faultStore) crashed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired && s.mode == modeCrash
}

func (s *faultStore) Scoped(scope store.Scope) (store.Repos, error) {
	repos, err := s.inner.Scoped(scope)
	if err != nil {
		return nil, err
	}
	return &faultRepos{Repos: repos, fs: s}, nil
}

func (s *faultStore) Atomic(ctx context.Context, scope store.Scope, opts store.TxOptions, fn func(ctx context.Context, tx store.Tx) error) error {
	return s.inner.Atomic(ctx, scope, opts, func(ctx context.Context, tx store.Tx) error {
		return fn(ctx, &faultTx{Tx: tx, fs: s})
	})
}

func (s *faultStore) Maintenance(scope store.Scope) (store.Maintenance, error) {
	return s.inner.Maintenance(scope)
}

func (s *faultStore) Migrator() store.Migrator { return s.inner.Migrator() }

func (s *faultStore) Capabilities(ctx context.Context) (store.Capabilities, error) {
	return s.inner.Capabilities(ctx)
}

func (s *faultStore) Close(ctx context.Context) error { return s.inner.Close(ctx) }

type faultRepos struct {
	store.Repos
	fs *faultStore
}

func (r *faultRepos) Runs() store.RunRepo {
	return &faultRuns{RunRepo: r.Repos.Runs(), fs: r.fs}
}

type faultTx struct {
	store.Tx
	fs *faultStore
}

func (t *faultTx) Runs() store.RunRepo {
	return &faultRuns{RunRepo: t.Tx.Runs(), fs: t.fs}
}

type faultRuns struct {
	store.RunRepo
	fs *faultStore
}

func (r *faultRuns) Checkpoint(ctx context.Context, id string, expectedStep int, state json.RawMessage) (domain.Run, error) {
	if fail, block := r.fs.intercept("Checkpoint"); fail {
		if block {
			<-r.fs.release
		}
		return domain.Run{}, errInjected
	}
	return r.RunRepo.Checkpoint(ctx, id, expectedStep, state)
}

func (r *faultRuns) SetStatus(ctx context.Context, id string, expectedStep int, status domain.RunStatus) (domain.Run, error) {
	if fail, block := r.fs.intercept("SetStatus(" + string(status) + ")"); fail {
		if block {
			<-r.fs.release
		}
		return domain.Run{}, errInjected
	}
	return r.RunRepo.SetStatus(ctx, id, expectedStep, status)
}

func (r *faultRuns) Finish(ctx context.Context, id string, status domain.RunStatus, failure string) (domain.Run, error) {
	if fail, block := r.fs.intercept("Finish"); fail {
		if block {
			<-r.fs.release
		}
		return domain.Run{}, errInjected
	}
	return r.RunRepo.Finish(ctx, id, status, failure)
}

func (r *faultRuns) Heartbeat(ctx context.Context, id string, worker string) error {
	if r.fs.crashed() {
		return nil // a dead process sends no heartbeats
	}
	return r.RunRepo.Heartbeat(ctx, id, worker)
}

// faultFixture is a step fixture plus two managers over one store: the dead
// worker drives through the fault, the recovery worker reads the healthy
// inner store. They share the provider, history, clock, ID generator and
// event log — all mutex-protected — because a crash and its recovery observe
// one world from two processes.
type faultFixture struct {
	*managerFixture
	fault    *faultStore
	recovery agent.RunManager
}

func newFaultFixture(t *testing.T, target string, mode faultMode) *faultFixture {
	t.Helper()
	sf := newStepFixture(t)
	base := memory.New(memory.Options{Clock: sf.clock})
	t.Cleanup(func() { _ = base.Close(context.Background()) })
	fault := newFaultStore(base, target, mode)
	if mode == modeCrash {
		// t.Cleanup runs last-in-first-out, so registering the release after
		// the store close makes it run first: the blocked zombie wakes and
		// errors out before anything the test still observes goes away.
		t.Cleanup(func() { close(fault.release) })
	}

	emitter := &recordingEmitter{}
	resolver := sf.stepper.(agent.CallResolver)
	dead, err := agent.NewManager(agent.ManagerConfig{
		Store:    fault,
		Stepper:  sf.stepper,
		Resolver: resolver,
		Emitter:  emitter,
		Clock:    sf.clock,
		IDs:      sf.ids,
		Worker:   "dead-worker",
	})
	if err != nil {
		t.Fatalf("NewManager (dead worker): %v", err)
	}
	recovery, err := agent.NewManager(agent.ManagerConfig{
		Store:    base,
		Stepper:  sf.stepper,
		Resolver: resolver,
		Emitter:  emitter,
		Clock:    sf.clock,
		IDs:      sf.ids,
		Worker:   "recovery-worker",
	})
	if err != nil {
		t.Fatalf("NewManager (recovery worker): %v", err)
	}
	return &faultFixture{
		managerFixture: &managerFixture{
			stepFixture: sf,
			store:       base, // assertions and recovery read the healthy store
			emitter:     emitter,
			manager:     dead,
			resolver:    resolver,
			scope:       store.Scope{Owner: "u1"},
		},
		fault:    fault,
		recovery: recovery,
	}
}

// waitHit waits for the injected fault to fire instead of sleeping on it.
func waitHit(t *testing.T, hit chan struct{}, what string) {
	t.Helper()
	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s to hit the injected fault", what)
	}
}

// pendingCalls reads the approval queue out of a run's checkpointed state.
func pendingCalls(t *testing.T, state json.RawMessage) int {
	t.Helper()
	var st struct {
		Pending []json.RawMessage `json:"pending,omitempty"`
	}
	if len(state) > 0 {
		if err := json.Unmarshal(state, &st); err != nil {
			t.Fatalf("decode run state: %v", err)
		}
	}
	return len(st.Pending)
}

// countContains counts occurrences of want in list.
func countContains(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func TestTransientClaimFailureFailsTheRunBeforeAnyStep(t *testing.T) {
	f := newFaultFixture(t, "SetStatus(running)", modeError)
	f.prov.ReplyText("never runs")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "the claim")

	final := f.waitRunStatus(t, run.ID, domain.RunFailed)
	if !strings.Contains(final.Failure, "injected") {
		t.Errorf("failure = %q, want the injected store error", final.Failure)
	}
	if n := len(f.prov.Requests()); n != 0 {
		t.Errorf("provider called %d times, want 0 — the run died before its first step", n)
	}
	if n := len(f.hist.committed()); n != 0 {
		t.Errorf("committed %d turns, want 0", n)
	}

	// A terminal run is not recoverable: nothing may re-drive it.
	if err := f.recovery.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := f.run(t, run.ID); got.Status != domain.RunFailed {
		t.Errorf("after recovery status = %q, want failed", got.Status)
	}
}

func TestTransientCheckpointFailureFailsTheRunAndKeepsCommittedWork(t *testing.T) {
	f := newFaultFixture(t, "Checkpoint", modeError)
	f.prov.ReplyText("one")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "the step checkpoint")

	final := f.waitRunStatus(t, run.ID, domain.RunFailed)
	if !strings.Contains(final.Failure, "injected") {
		t.Errorf("failure = %q, want the injected store error", final.Failure)
	}
	if final.Step != 0 {
		t.Errorf("step = %d, want 0 — the checkpoint never landed", final.Step)
	}
	if n := len(f.hist.committed()); n != 1 {
		t.Errorf("committed %d turns, want 1 — the step's turn survives its failed checkpoint", n)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("provider called %d times, want 1", n)
	}

	// Recovery must not resurrect a terminal run or re-commit its work.
	if err := f.recovery.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := f.run(t, run.ID); got.Status != domain.RunFailed {
		t.Errorf("after recovery status = %q, want failed", got.Status)
	}
	if n := len(f.hist.committed()); n != 1 {
		t.Errorf("after recovery committed %d turns, want 1", n)
	}
}

func TestTransientParkingFailureFailsTheRunWithTheCallStillPending(t *testing.T) {
	f := newFaultFixture(t, "SetStatus(waiting)", modeError)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "ask", Risk: agent.RiskConfirm}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call-1", Name: "ask", Arguments: json.RawMessage(`{}`)})

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "the park")

	final := f.waitRunStatus(t, run.ID, domain.RunFailed)
	if !strings.Contains(final.Failure, "injected") {
		t.Errorf("failure = %q, want the injected store error", final.Failure)
	}
	if final.Step != 1 {
		t.Errorf("step = %d, want 1 — the step committed before the park failed", final.Step)
	}
	if n := pendingCalls(t, final.State); n != 1 {
		t.Errorf("pending calls = %d, want 1 — the checkpointed call survives the failed park", n)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("provider called %d times, want 1", n)
	}
	for _, ev := range f.rec.all() {
		if ev == "tool:ask" {
			t.Fatal("the RiskConfirm tool ran without approval")
		}
	}
	// A failed run cannot be resumed by an approval.
	if err := f.manager.Approve(context.Background(), testPrincipal(), run.ID, "call-1", true, ""); !kernel.IsCode(err, kernel.CodeConflict) {
		t.Errorf("approving a failed run err = %v, want CONFLICT", err)
	}
}

func TestTransientFinishFailureIsRetriedByTheLoop(t *testing.T) {
	f := newFaultFixture(t, "Finish", modeError)
	f.prov.ReplyText("one")
	f.prov.ReplyText("two")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "Finish")

	final := f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	if final.Failure != "" {
		t.Errorf("failure = %q, want none — a swallowed Finish error is not a failed run", final.Failure)
	}
	// The failed Finish write leaves the run running; the loop's redrive
	// picks it up and repeats exactly the one step in flight.
	if n := len(f.prov.Requests()); n != 2 {
		t.Errorf("provider called %d times, want 2", n)
	}
	if n := len(f.hist.committed()); n != 2 {
		t.Errorf("committed %d turns, want 2 — no committed turn is lost", n)
	}
	if final.Step != 2 {
		t.Errorf("step = %d, want 2", final.Step)
	}
}

func TestCrashAtClaimIsRecoveredByAnotherWorker(t *testing.T) {
	f := newFaultFixture(t, "SetStatus(running)", modeCrash)
	f.prov.ReplyText("recovered")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "the claim")

	// The crashed write never landed: the run is still queued.
	got := f.run(t, run.ID)
	if got.Status != domain.RunQueued {
		t.Errorf("status = %q, want queued — the crashed claim must not have landed", got.Status)
	}
	if n := len(f.prov.Requests()); n != 0 {
		t.Errorf("provider called %d times, want 0", n)
	}

	// A queued run is claimable without waiting out the stale window.
	if err := f.recovery.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	final := f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	if final.Failure != "" {
		t.Errorf("failure = %q", final.Failure)
	}
	if final.Worker != "recovery-worker" {
		t.Errorf("worker = %q, want recovery-worker", final.Worker)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("provider called %d times, want 1", n)
	}
	if n := len(f.hist.committed()); n != 1 {
		t.Errorf("committed %d turns, want 1", n)
	}
}

func TestCrashAtCheckpointRepeatsTheInFlightStepOnce(t *testing.T) {
	f := newFaultFixture(t, "Checkpoint", modeCrash)
	f.prov.ReplyText("one")
	f.prov.ReplyText("two")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "the step checkpoint")

	got := f.run(t, run.ID)
	if got.Status != domain.RunRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.Step != 0 {
		t.Errorf("step = %d, want 0 — the crashed checkpoint never landed", got.Step)
	}
	if n := len(f.hist.committed()); n != 1 {
		t.Errorf("committed %d turns, want 1 — the step's turn survives its crashed checkpoint", n)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("provider called %d times, want 1", n)
	}

	// Outlive the stale window so recovery can claim the dead worker's run.
	f.clock.Advance(time.Hour)
	if err := f.recovery.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	final := f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	if final.Failure != "" {
		t.Errorf("failure = %q", final.Failure)
	}
	if final.Worker != "recovery-worker" {
		t.Errorf("worker = %q, want recovery-worker", final.Worker)
	}
	// Exactly the in-flight step repeats: one extra model call, one extra
	// turn, and the step that never landed lands once.
	if n := len(f.prov.Requests()); n != 2 {
		t.Errorf("provider called %d times, want 2 — exactly the in-flight step repeats", n)
	}
	if n := len(f.hist.committed()); n != 2 {
		t.Errorf("committed %d turns, want 2 — no committed turn is lost", n)
	}
	if final.Step != 1 {
		t.Errorf("step = %d, want 1 — one checkpoint landed", final.Step)
	}
}

func TestCrashWhileParkingIsRecoveredByReparkingAndApprovalRunsTheCall(t *testing.T) {
	f := newFaultFixture(t, "SetStatus(waiting)", modeCrash)
	f.register(t, countingTool(f.rec, agent.ToolSpec{Name: "ask", Risk: agent.RiskConfirm}))
	f.prov.ReplyToolCalls(provider.ToolCallPart{CallID: "call-1", Name: "ask", Arguments: json.RawMessage(`{}`)})
	f.prov.ReplyText("after approval")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "the park")

	// The checkpoint that folded the call into the run's state landed; only
	// the status write was lost, so the run is running with a pending call.
	got := f.run(t, run.ID)
	if got.Status != domain.RunRunning {
		t.Errorf("status = %q, want running — the crashed park must not have landed", got.Status)
	}
	if got.Step != 1 {
		t.Errorf("step = %d, want 1 — the step checkpoint before the park landed", got.Step)
	}
	if n := pendingCalls(t, got.State); n != 1 {
		t.Errorf("pending calls = %d, want 1", n)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("provider called %d times, want 1", n)
	}
	for _, ev := range f.rec.all() {
		if ev == "tool:ask" {
			t.Fatal("the RiskConfirm tool ran before approval")
		}
	}

	// Outlive the stale window so recovery can claim the dead worker's run.
	f.clock.Advance(time.Hour)
	if err := f.recovery.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Recovery re-parks instead of stepping past a call nobody approved.
	parked := f.waitRunStatus(t, run.ID, domain.RunWaiting)
	if parked.Step != 1 {
		t.Errorf("after recovery step = %d, want 1", parked.Step)
	}
	if n := pendingCalls(t, parked.State); n != 1 {
		t.Errorf("after recovery pending calls = %d, want 1", n)
	}
	f.waitEvent(t, "run.approval_requested")
	if n := countContains(f.emitter.names(), "run.approval_requested"); n != 1 {
		t.Errorf("approval_requested emitted %d times, want 1", n)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("provider called %d times before approval, want 1", n)
	}
	for _, ev := range f.rec.all() {
		if ev == "tool:ask" {
			t.Fatal("the RiskConfirm tool ran before approval")
		}
	}

	if err := f.recovery.Approve(context.Background(), testPrincipal(), run.ID, "call-1", true, "looks fine"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	f.waitEvent(t, "run.resumed")
	final := f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	if final.Failure != "" {
		t.Errorf("failure = %q", final.Failure)
	}
	if n := len(f.prov.Requests()); n != 2 {
		t.Errorf("provider called %d times, want 2", n)
	}
	if n := countContains(f.rec.all(), "tool:ask"); n != 1 {
		t.Errorf("the approved tool ran %d times, want exactly 1", n)
	}
}

func TestCrashAtFinishIsRecoveredByAnotherWorker(t *testing.T) {
	f := newFaultFixture(t, "Finish", modeCrash)
	f.prov.ReplyText("one")
	f.prov.ReplyText("two")

	run, err := f.manager.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{Model: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHit(t, f.fault.hit, "Finish")

	got := f.run(t, run.ID)
	if got.Status != domain.RunRunning {
		t.Errorf("status = %q, want running — the crashed Finish must not have landed", got.Status)
	}
	if got.Step != 1 {
		t.Errorf("step = %d, want 1 — the step committed before Finish crashed", got.Step)
	}
	if n := len(f.hist.committed()); n != 1 {
		t.Errorf("committed %d turns, want 1", n)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("provider called %d times, want 1", n)
	}

	// Outlive the stale window so recovery can claim the dead worker's run.
	f.clock.Advance(time.Hour)
	if err := f.recovery.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	final := f.waitRunStatus(t, run.ID, domain.RunSucceeded)
	if final.Failure != "" {
		t.Errorf("failure = %q", final.Failure)
	}
	if final.Worker != "recovery-worker" {
		t.Errorf("worker = %q, want recovery-worker", final.Worker)
	}
	// The terminal write was lost, so recovery repeats the one step whose
	// Finish never landed; the committed work of the first step survives.
	if n := len(f.prov.Requests()); n != 2 {
		t.Errorf("provider called %d times, want 2", n)
	}
	if n := len(f.hist.committed()); n != 2 {
		t.Errorf("committed %d turns, want 2 — no committed turn is lost", n)
	}
	if final.Step != 2 {
		t.Errorf("step = %d, want 2", final.Step)
	}
}
