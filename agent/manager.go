package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// Defaults from sections 11 and 15: a run heartbeats every 10 seconds and is
// considered abandoned after three missed beats.
const (
	DefaultHeartbeatInterval = 10 * time.Second
	defaultStaleBeats        = 3
	// approvalWindow is how long a RiskConfirm call may wait for a person
	// before it lapses into an error result (section 11, approval).
	approvalWindow = 24 * time.Hour
)

// The manager's events, named as section 13's command table requires.
// Events for a terminal run are not invented here: the table names
// run.started, run.cancelled and run.resumed for the three commands, and a
// terminal state is read from the store rather than announced.

// RunStarted is emitted once a run is created and before it is driven, so a
// subscriber never misses the beginning of a run it is about to watch.
type RunStarted struct {
	RunID string
}

// EventName implements kernel.Event.
func (RunStarted) EventName() string { return "run.started" }

// ApprovalRequested is emitted when a RiskConfirm call parks a run. Approving
// or rejecting resumes it with run.resumed.
type ApprovalRequested struct {
	RunID  string
	CallID string
	Tool   string
}

// EventName implements kernel.Event.
func (ApprovalRequested) EventName() string { return "run.approval_requested" }

// RunResumed is emitted when a parked run starts moving again, whether the
// pending call was approved or rejected.
type RunResumed struct {
	RunID    string
	CallID   string
	Approved bool
	Reason   string
}

// EventName implements kernel.Event.
func (RunResumed) EventName() string { return "run.resumed" }

// RunCancelled is emitted after a cancellation has been committed.
type RunCancelled struct {
	RunID string
}

// EventName implements kernel.Event.
func (RunCancelled) EventName() string { return "run.cancelled" }

// Emitter publishes an event about state that has already been committed.
// kernel.Emitter is satisfied by *kernel.Kernel, which dispatch uses for
// command handlers and the agent loop uses for the rest.
type Emitter interface {
	Emit(e kernel.Event, owner, conversationID string)
}

// CallResolver finishes one pending call: it runs the tool when the call is
// approved, or commits an error result when it is rejected or left to lapse,
// so the model can adapt either way (section 11, approval). The turn it
// returns is the committed result.
type CallResolver interface {
	Resolve(ctx context.Context, p kernel.Principal, conversationID string, parent domain.TurnRef, call ToolCall, approve bool, reason string) (domain.Turn, error)
}

// RunManager is Manager plus the two interfaces section 11 says the manager
// also implements: "the manager implements both interfaces", so a custom loop
// drives its steps and its crash-recovery checkpoints through the same object
// that owns the runs. NewManager returns this rather than the bare Manager
// the spec declares, because the concrete type does implement all three.
type RunManager interface {
	Manager
	Stepper
	Checkpointer
}

// ManagerConfig is what the Manager needs. Store, Stepper, Resolver, Clock
// and IDs are required; Emitter is optional.
type ManagerConfig struct {
	Store      store.Store
	Stepper    Stepper
	Resolver   CallResolver
	Emitter    Emitter
	Clock      domain.Clock
	IDs        domain.IDGenerator
	Worker     string
	Heartbeat  time.Duration
	StaleAfter time.Duration
}

// pendingCall is one parked call plus the turn its result will hang off.
type pendingCall struct {
	Call        ToolCall       `json:"call"`
	Parent      domain.TurnRef `json:"parent"`
	RequestedAt time.Time      `json:"requested_at"`
}

// runState is the loop's own checkpoint payload: everything a restart needs
// to resume a run. It is opaque to the store, which is exactly what section
// 6's State field is for.
type runState struct {
	Principal  kernel.Principal `json:"principal"`
	Options    RunOptions       `json:"options"`
	CostMicros int64            `json:"cost_micros"`
	Pending    []pendingCall    `json:"pending,omitempty"`
}

// EncodeRunState returns the checkpoint payload a run must carry to be
// resumable: who it runs as, the limits that bound it and anything parked.
// The manager writes these bytes itself; a host that seeds a run directly —
// a crash-recovery test, a migration, a resumed deployment — uses this so
// the format stays one thing.
func EncodeRunState(p kernel.Principal, opts RunOptions) (json.RawMessage, error) {
	if p.Owner == "" {
		return nil, kernel.NewError(kernel.CodeInvalid, "a run state needs an owner")
	}
	return json.Marshal(runState{Principal: p, Options: withRunDefaults(opts)})
}

type manager struct {
	store     store.Store
	stepper   Stepper
	resolver  CallResolver
	emitter   Emitter
	clock     domain.Clock
	ids       domain.IDGenerator
	worker    string
	heartbeat time.Duration
	stale     time.Duration

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	driving  map[string]bool
	finishMu sync.Mutex
}

// NewManager builds the run manager: the policy over the step primitive that
// owns run states, limits, approval and recovery. It also implements
// Stepper and Checkpointer, so a custom loop can drive its own steps through
// the same manager and still get durable runs.
func NewManager(cfg ManagerConfig) (RunManager, error) {
	if cfg.Store == nil || cfg.Stepper == nil || cfg.Resolver == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "a manager needs a store, a stepper and a resolver")
	}
	if cfg.Clock == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "a clock is required; nothing in the core calls time.Now")
	}
	if cfg.IDs == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "an ID generator is required")
	}
	if cfg.Worker == "" {
		cfg.Worker = "workspace"
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeatInterval
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = defaultStaleBeats * cfg.Heartbeat
	}
	return &manager{
		store:     cfg.Store,
		stepper:   cfg.Stepper,
		resolver:  cfg.Resolver,
		emitter:   cfg.Emitter,
		clock:     cfg.Clock,
		ids:       cfg.IDs,
		worker:    cfg.Worker,
		heartbeat: cfg.Heartbeat,
		stale:     cfg.StaleAfter,
		cancels:   map[string]context.CancelFunc{},
		driving:   map[string]bool{},
	}, nil
}

// Step and Checkpoint let the manager stand in as the step primitive, so a
// custom loop drives steps through one object and gets runs, limits and
// recovery for free.
func (m *manager) Step(ctx context.Context, p kernel.Principal, conversationID string, opts StepOptions) (StepResult, error) {
	return m.stepper.Step(ctx, p, conversationID, opts)
}

func (m *manager) Checkpoint(ctx context.Context, p kernel.Principal, runID string, expectedStep int, state json.RawMessage) (domain.Run, error) {
	repos, err := m.repos(p.Owner)
	if err != nil {
		return domain.Run{}, err
	}
	if err := m.authorize(ctx, p, repos, runID); err != nil {
		return domain.Run{}, err
	}
	run, err := repos.Runs().Checkpoint(ctx, runID, expectedStep, state)
	return run, kernel.FromStore(err)
}

// Start creates a run in the queued state, announces it and starts driving
// it. The loop owns its context: neither Start's caller nor a later Cancel
// caller can cancel the run's work by dropping their own context.
func (m *manager) Start(ctx context.Context, p kernel.Principal, conversationID string, opts RunOptions) (domain.Run, error) {
	if conversationID == "" {
		return domain.Run{}, kernel.NewError(kernel.CodeInvalid, "a conversation ID is required")
	}
	if p.Owner == "" {
		return domain.Run{}, kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}

	repos, err := m.repos(p.Owner)
	if err != nil {
		return domain.Run{}, kernel.FromStore(err)
	}
	if err := ctx.Err(); err != nil {
		return domain.Run{}, kernel.WrapError(kernel.CodeCanceled, "start cancelled", err)
	}

	state, err := EncodeRunState(p, opts)
	if err != nil {
		return domain.Run{}, err
	}
	run, err := repos.Runs().Create(ctx, domain.Run{
		ID:             m.ids.NewID(),
		ConversationID: conversationID,
		Status:         domain.RunQueued,
		State:          state,
		Worker:         m.worker,
	})
	if err != nil {
		return domain.Run{}, err
	}
	m.emit(RunStarted{RunID: run.ID}, p.Owner, conversationID)
	m.drive(run.ID)
	return run, nil
}

// Cancel stops a run by cancelling the context the manager owns. The
// caller's request context is untouched: cancelling it would leave a run
// half-committed with no way to tell who cancelled what.
func (m *manager) Cancel(ctx context.Context, p kernel.Principal, runID string) error {
	repos, err := m.repos(p.Owner)
	if err != nil {
		return err
	}
	run, err := repos.Runs().Get(ctx, runID)
	if err != nil {
		return kernel.WrapError(kernel.CodeNotFound, "run not found", err)
	}
	// Authorization first: a caller who does not own the run must not learn
	// its state from which error code comes back.
	if err := m.authorizeRun(p, run); err != nil {
		return err
	}
	if run.Status.IsTerminal() {
		return kernel.NewError(kernel.CodeConflict, "run already finished")
	}

	// Cancel the loop first so it stops committing, then commit the state.
	if cancel, ok := m.stopDriving(runID); ok {
		cancel()
	}
	run, committed, err := m.commitTerminal(runID, domain.RunCancelled, "cancelled by operator")
	if err != nil {
		return kernel.FromStore(err)
	}
	if !committed {
		// The loop won the race and finished the run while we were
		// cancelling; it announced its own terminal event. Cancelling a
		// run that is already cancelled is a no-op, not a conflict.
		if run.Status == domain.RunCancelled {
			return nil
		}
		return kernel.NewError(kernel.CodeConflict, "run already finished")
	}
	m.emit(RunCancelled{RunID: runID}, p.Owner, run.ConversationID)
	return nil
}

// Approve or reject a parked call. Approving runs it; rejecting, or arriving
// after the approval window has lapsed, commits an error result so the model
// is told no rather than left waiting. Either way the run resumes.
func (m *manager) Approve(ctx context.Context, p kernel.Principal, runID, callID string, approve bool, reason string) error {
	repos, err := m.repos(p.Owner)
	if err != nil {
		return err
	}
	run, err := repos.Runs().Get(ctx, runID)
	if err != nil {
		return kernel.WrapError(kernel.CodeNotFound, "run not found", err)
	}
	if err := m.authorizeRun(p, run); err != nil {
		return err
	}
	if run.Status != domain.RunWaiting {
		return kernel.NewError(kernel.CodeConflict, "run is not waiting for approval")
	}

	var st runState
	if len(run.State) > 0 {
		if err := json.Unmarshal(run.State, &st); err != nil {
			return kernel.WrapError(kernel.CodeInternal, "run state is corrupt", err)
		}
	}
	idx := -1
	for i := range st.Pending {
		if st.Pending[i].Call.CallID == callID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return kernel.NewError(kernel.CodeNotFound, "no pending call with that ID")
	}
	pending := st.Pending[idx]

	// A lapsed window becomes a rejection, so a forgotten approval does not
	// block the run forever.
	if m.clock.Now().Sub(pending.RequestedAt) > approvalWindow {
		approve = false
		if reason == "" {
			reason = "approval window lapsed"
		}
	}

	turn, err := m.resolver.Resolve(ctx, p, run.ConversationID, pending.Parent, pending.Call, approve, reason)
	if err != nil {
		return err
	}
	_ = turn

	st.Pending = append(st.Pending[:idx], st.Pending[idx+1:]...)
	state, err := json.Marshal(st)
	if err != nil {
		return kernel.WrapError(kernel.CodeInternal, "cannot encode the run state", err)
	}
	if _, err := repos.Runs().Checkpoint(ctx, runID, run.Step, state); err != nil {
		return kernel.FromStore(err)
	}
	if _, err := repos.Runs().SetStatus(ctx, runID, run.Step+1, domain.RunRunning); err != nil {
		return kernel.FromStore(err)
	}

	m.emit(RunResumed{RunID: runID, CallID: callID, Approved: approve, Reason: reason}, p.Owner, run.ConversationID)
	m.drive(run.ID)
	return nil
}

// Recover claims abandoned runs — those whose heartbeat has gone quiet — and
// resumes them. The store's claim gives exactly one claimant per run, so two
// processes recovering at the same time cannot both drive a run.
func (m *manager) Recover(ctx context.Context) error {
	repos, err := m.reposSystem()
	if err != nil {
		return err
	}
	staleBefore := m.clock.Now().Add(-m.stale)
	claimed, err := repos.Runs().Claim(ctx, staleBefore, defaultRecoveryLimit, m.worker)
	if err != nil {
		return kernel.FromStore(err)
	}
	for _, run := range claimed {
		// A claimed run is running by definition; its state says what it was
		// doing, including whether it was parked on a person.
		m.drive(run.ID)
	}
	return nil
}

// defaultRecoveryLimit bounds one recovery pass, so a backlog cannot starve
// the caller that asked for it.
const defaultRecoveryLimit = 32

// drive runs one loop on its own goroutine. A run already being driven here
// is left alone: Recover can claim a run another process's loop still holds,
// and the CAS on the step keeps the two from both committing.
func (m *manager) drive(runID string) {
	m.mu.Lock()
	if m.driving[runID] {
		m.mu.Unlock()
		return
	}
	m.driving[runID] = true
	m.mu.Unlock()

	go m.loop(runID)
}

// redrive picks a run back up when it was resumed in the instant between a
// loop's last write and that goroutine clearing its driving flag: Approve's
// drive call sees the flag still set and skips, and without this check
// nothing would ever move the run again until recovery reclaimed it.
func (m *manager) redrive(runID string) {
	repos, err := m.reposSystem()
	if err != nil {
		return
	}
	run, err := repos.Runs().Get(context.Background(), runID)
	if err != nil {
		return
	}
	if run.Status == domain.RunRunning {
		m.drive(runID)
	}
}

func (m *manager) loop(runID string) {
	ctx, cancel := m.runContext()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.driving, runID)
		delete(m.cancels, runID)
		m.mu.Unlock()
		m.redrive(runID)
	}()
	m.mu.Lock()
	m.cancels[runID] = cancel
	m.mu.Unlock()

	// The heartbeat ticker keeps a run that is blocked inside a long provider
	// call from looking abandoned. It runs beside the loop rather than at
	// each step because a single call may exceed the stale window.
	ticks := time.NewTicker(m.heartbeat)
	defer ticks.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-ticks.C:
				m.heartbeatRun(runID)
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	repos, err := m.reposSystem()
	if err != nil {
		m.fail(runID, "", err)
		return
	}
	run, err := repos.Runs().Get(ctx, runID)
	if err != nil {
		m.fail(runID, "", err)
		return
	}
	var st runState
	if len(run.State) > 0 {
		if err := json.Unmarshal(run.State, &st); err != nil {
			m.fail(runID, "", err)
			return
		}
	}
	if run.Status == domain.RunQueued {
		if _, err := repos.Runs().SetStatus(ctx, runID, run.Step, domain.RunRunning); err != nil {
			m.fail(runID, "", err)
			return
		}
	}

	// A crash between the checkpoint that parked its calls and the status
	// write that made the wait visible leaves the run running with committed
	// pending calls. Re-park rather than step past a call nobody approved.
	if len(st.Pending) > 0 {
		if _, err := repos.Runs().SetStatus(ctx, runID, run.Step, domain.RunWaiting); err != nil {
			m.fail(runID, st.Principal.Owner, err)
			return
		}
		for _, call := range st.Pending {
			m.emit(ApprovalRequested{RunID: runID, CallID: call.Call.CallID, Tool: call.Call.Name},
				st.Principal.Owner, run.ConversationID)
		}
		return
	}

	stepOpts := StepOptions{
		Model:        st.Options.Model,
		MaxParallel:  st.Options.MaxParallel,
		Instructions: st.Options.Instructions,
		Tools:        st.Options.Tools,
	}
	deadline := time.Time{}
	if st.Options.Deadline > 0 {
		deadline = m.clock.Now().Add(st.Options.Deadline)
	}

	for {
		// The three caps, each with its own code so a failed run says why.
		if st.Options.MaxSteps > 0 && run.Step >= st.Options.MaxSteps {
			m.fail(runID, st.Principal.Owner, kernel.NewError(kernel.CodeInvalid,
				"step budget exhausted"))
			return
		}
		if !deadline.IsZero() && m.clock.Now().After(deadline) {
			m.fail(runID, st.Principal.Owner, kernel.NewError(kernel.CodeCanceled,
				"run deadline exceeded"))
			return
		}
		if st.Options.CostCapMicros > 0 && st.CostMicros > st.Options.CostCapMicros {
			m.fail(runID, st.Principal.Owner, kernel.NewError(kernel.CodeInvalid,
				"cost cap exceeded"))
			return
		}
		if err := ctx.Err(); err != nil {
			m.finish(runID, st.Principal.Owner, domain.RunCancelled, "cancelled")
			return
		}

		res, err := m.stepper.Step(ctx, st.Principal, run.ConversationID, stepOpts)
		run.Step++
		st.CostMicros += res.CostMicros

		// One checkpoint per step, carrying everything the next run of the
		// loop needs: the spend, and any calls that parked. Checkpointing
		// twice would advance the counter twice, which is why the pending
		// calls are folded in here rather than saved separately.
		st.Pending = nil
		if err == nil && len(res.Pending) > 0 {
			parent := res.Turns[len(res.Turns)-1].Ref()
			now := m.clock.Now()
			for _, call := range res.Pending {
				st.Pending = append(st.Pending, pendingCall{Call: call, Parent: parent, RequestedAt: now})
			}
		}
		if err == nil {
			var state []byte
			state, err = json.Marshal(st)
			if err == nil {
				_, err = repos.Runs().Checkpoint(ctx, runID, run.Step-1, state)
			}
		}
		if err != nil {
			if ctx.Err() != nil || kernel.IsCode(err, kernel.CodeCanceled) {
				// Uncommitted partial work was already discarded by the step.
				m.finish(runID, st.Principal.Owner, domain.RunCancelled, "cancelled")
				return
			}
			m.fail(runID, st.Principal.Owner, err)
			return
		}

		// The cost cap is checked the moment the spend is committed, not only
		// before the next step: a run that has already overshot must not
		// report success.
		if st.Options.CostCapMicros > 0 && st.CostMicros > st.Options.CostCapMicros {
			m.fail(runID, st.Principal.Owner, kernel.NewError(kernel.CodeInvalid,
				"cost cap exceeded"))
			return
		}

		if len(res.Pending) > 0 {
			if _, err := repos.Runs().SetStatus(ctx, runID, run.Step, domain.RunWaiting); err != nil {
				m.fail(runID, st.Principal.Owner, err)
				return
			}
			for _, call := range st.Pending {
				m.emit(ApprovalRequested{RunID: runID, CallID: call.Call.CallID, Tool: call.Call.Name},
					st.Principal.Owner, run.ConversationID)
			}
			return // parked: Approve or Cancel is what moves it now
		}

		if res.Finish != "" && res.Finish != provider.FinishToolCalls {
			m.finish(runID, st.Principal.Owner, domain.RunSucceeded, "")
			return
		}
		// The model asked for tools and every one of them ran: step again.
	}
}

// runContext is the context the loop owns. It is derived from nothing the
// caller controls, so dropping a request context mid-run cannot strand a run
// in a half-committed state.
func (m *manager) runContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func (m *manager) finish(runID, owner string, status domain.RunStatus, failure string) {
	run, committed, err := m.commitTerminal(runID, status, failure)
	if err != nil || !committed {
		return // already finished: nothing to write and nothing to announce
	}
	if status == domain.RunCancelled {
		m.emit(RunCancelled{RunID: runID}, owner, run.ConversationID)
	}
}

// commitTerminal moves a run to a terminal status at most once. Cancel and the
// loop can both reach the same run (Cancel cancels the context, the loop wakes
// up on it), so the read and the write are serialised: whoever loses sees the
// terminal status and does not write or announce a second time.
func (m *manager) commitTerminal(runID string, status domain.RunStatus, failure string) (domain.Run, bool, error) {
	m.finishMu.Lock()
	defer m.finishMu.Unlock()
	repos, err := m.reposSystem()
	if err != nil {
		return domain.Run{}, false, err
	}
	run, err := repos.Runs().Get(context.Background(), runID)
	if err != nil {
		return domain.Run{}, false, err
	}
	if run.Status.IsTerminal() {
		return run, false, nil
	}
	finished, err := repos.Runs().Finish(context.Background(), runID, status, failure)
	if err != nil {
		return domain.Run{}, false, err
	}
	return finished, true, nil
}

func (m *manager) fail(runID, owner string, cause error) {
	m.finish(runID, owner, domain.RunFailed, failureText(cause))
}

func (m *manager) heartbeatRun(runID string) {
	repos, err := m.reposSystem()
	if err != nil {
		return
	}
	_ = repos.Runs().Heartbeat(context.Background(), runID, m.worker)
}

// stopDriving cancels and forgets a run's context, reporting whether this
// call was the one that stopped it.
func (m *manager) stopDriving(runID string) (context.CancelFunc, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cancel, ok := m.cancels[runID]
	if ok {
		delete(m.cancels, runID)
	}
	return cancel, ok
}

func (m *manager) emit(e kernel.Event, owner, conversationID string) {
	if m.emitter == nil || e == nil {
		return
	}
	m.emitter.Emit(e, owner, conversationID)
}

// repos returns the owner-scoped repository set. Runs are not owner-scoped
// in the port — a run points at a conversation, which is what scopes it — so
// the owner is checked against the run rather than by the repository.
func (m *manager) repos(owner string) (store.Repos, error) {
	if owner == "" {
		return nil, kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	return m.store.Scoped(store.Scope{Owner: owner})
}

func (m *manager) reposSystem() (store.Repos, error) {
	return m.store.Scoped(store.Scope{System: true})
}

// authorizeRun checks that the principal owns the run, which is the check
// the port itself does not do because a run carries no owner of its own.
func (m *manager) authorizeRun(p kernel.Principal, run domain.Run) error {
	var st runState
	if len(run.State) > 0 {
		_ = json.Unmarshal(run.State, &st)
	}
	if st.Principal.Owner != "" && st.Principal.Owner != p.Owner {
		return kernel.NewError(kernel.CodeForbidden, "not the run's owner")
	}
	if p.Owner == "" {
		return kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	return nil
}

func (m *manager) authorize(ctx context.Context, p kernel.Principal, repos store.Repos, runID string) error {
	run, err := repos.Runs().Get(ctx, runID)
	if err != nil {
		return kernel.WrapError(kernel.CodeNotFound, "run not found", err)
	}
	return m.authorizeRun(p, run)
}

func failureText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// withRunDefaults applies section 11's limits: 25 steps, a 10-minute
// deadline and four parallel tool calls.
func withRunDefaults(opts RunOptions) RunOptions {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = DefaultMaxSteps
	}
	if opts.Deadline <= 0 {
		opts.Deadline = DefaultDeadline
	}
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = DefaultMaxParallel
	}
	return opts
}
