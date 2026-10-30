package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/memory"
)

// --- test doubles, local so the kernel tests do not import workspacetest ---
// (workspacetest imports kernel, so importing it back would be a cycle).

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type seqIDs struct{ n int }

func (g *seqIDs) NewID() string { g.n++; return "id-" + itoa(g.n) }

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

// --- commands used across the dispatch tests ---

type thingCreated struct{ ID string }

func (thingCreated) EventName() string { return "thing.created" }

type createThing struct{ Title string }

func (createThing) CommandName() string { return "thing.create" }
func (createThing) Target() Target {
	return Target{Kind: "conversation", ID: "conv-1"}
}

type ownerThing struct{ Title string }

func (ownerThing) CommandName() string { return "thing.owner" }

type thingResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// newTestKernel builds a kernel over the memory store with injected clock and
// IDs and a silent logger, per section 16: no network, no real clock, no
// real database.
func newTestKernel(t *testing.T, opts ...Option) (*Kernel, *fakeClock, store.Store) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	st := memory.New(memory.Options{Clock: clock})
	all := append([]Option{
		WithAuthorizer(AllowOwner()),
		WithClock(clock),
		WithIDs(&seqIDs{}),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}, opts...)
	k, err := New(st, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return k, clock, st
}

func testPrincipal() Principal {
	return Principal{Owner: "u1", Subject: "u1", Kind: UserPrincipal}
}

// waitUntil spins on a condition without sleeping. Section 16 forbids sleeps
// in tests; this is a condition wait that fails the test rather than hanging.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// --- New ---

func TestNewRequiresAuthorizer(t *testing.T) {
	st := memory.New(memory.Options{})
	if _, err := New(st, WithClock(&fakeClock{}), WithIDs(&seqIDs{})); !IsCode(err, CodeInvalid) {
		t.Fatalf("New without an authorizer err = %v, want INVALID", err)
	}
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := New(nil, WithAuthorizer(AllowOwner())); !IsCode(err, CodeInvalid) {
		t.Fatalf("New(nil store) err = %v, want INVALID", err)
	}
}

// storeConversation builds a conversation the handler can write, so a test
// can prove the write did or did not survive the transaction.
func storeConversation(id string) domain.Conversation {
	return domain.Conversation{ID: id, Owner: "u1", Title: "t"}
}

// --- dispatch pipeline ---

func TestDispatchSuccessPublishesAfterCommit(t *testing.T) {
	k, _, _ := newTestKernel(t)
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		return thingResult{ID: "t1", Title: c.Title}, []Event{thingCreated{ID: "t1"}}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := k.Bus().Subscribe(ctx, EventFilter{Owner: "u1"}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	got, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{Title: "hello"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got.Title != "hello" || got.ID != "t1" {
		t.Fatalf("result = %+v", got)
	}

	// Publish is synchronous, so the event is already queued.
	env := <-sub
	if env.Name != "thing.created" || env.Owner != "u1" || env.ConversationID != "conv-1" {
		t.Fatalf("event = %+v, want thing.created for u1/conv-1", env)
	}
}

func TestDispatchDeniesPrincipalWithoutOwner(t *testing.T) {
	k, _, _ := newTestKernel(t)
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		t.Error("handler must not run for an unauthenticated principal")
		return thingResult{}, nil, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := Dispatch[createThing, thingResult](context.Background(), k, Principal{}, createThing{})
	if !IsCode(err, CodeForbidden) {
		t.Fatalf("err = %v, want FORBIDDEN", err)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	k, _, _ := newTestKernel(t)
	_, err := Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{})
	if !IsCode(err, CodeNotFound) {
		t.Fatalf("err = %v, want NOT_FOUND", err)
	}
}

func TestDispatchRollbackPublishesNothing(t *testing.T) {
	k, _, st := newTestKernel(t)
	boom := errors.New("boom")
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		// A write that must not survive the rollback.
		if _, err := tx.Conversations().Create(ctx, storeConversation("conv-1")); err != nil {
			return thingResult{}, nil, err
		}
		return thingResult{}, []Event{thingCreated{}}, boom
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := k.Bus().Subscribe(ctx, EventFilter{}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if _, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom to propagate", err)
	}

	repos, _ := st.Scoped(store.Scope{Owner: "u1"})
	if _, err := repos.Conversations().Get(context.Background(), "conv-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back write is visible: %v", err)
	}
	select {
	case env := <-sub:
		t.Fatalf("a rolled-back command published %q", env.Name)
	default:
	}
}

func TestDispatchIdempotentReplay(t *testing.T) {
	k, _, _ := newTestKernel(t)
	calls := 0
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		calls++
		return thingResult{ID: "t1", Title: c.Title}, []Event{thingCreated{ID: "t1"}}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx := context.Background()
	first, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{Title: "hello"}, WithIdempotencyKey("k1"))
	if err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	second, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{Title: "hello"}, WithIdempotencyKey("k1"))
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
	if first != second {
		t.Fatalf("replay changed the result: %+v vs %+v", first, second)
	}
}

func TestDispatchIdempotentInFlightIsConflict(t *testing.T) {
	k, _, st := newTestKernel(t)
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		t.Error("handler must not run while an identical command is in flight")
		return thingResult{}, nil, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Reserve the key through the store, simulating a command that is still
	// running elsewhere: a reservation with no stored result.
	repos, err := st.Scoped(store.Scope{Owner: "u1"})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	if _, created, err := repos.Receipts().Reserve(context.Background(), "k2", time.Hour); err != nil || !created {
		t.Fatalf("Reserve = created %v err %v", created, err)
	}

	_, err = Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{}, WithIdempotencyKey("k2"))
	if !IsCode(err, CodeConflict) {
		t.Fatalf("err = %v, want CONFLICT", err)
	}
	var se *Error
	if !errors.As(err, &se) || !se.IsRetryable() {
		t.Fatalf("an in-flight duplicate must be retryable, got %v", err)
	}
}

func TestDispatchRecoversPanic(t *testing.T) {
	k, _, _ := newTestKernel(t)
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		var m map[string]string
		m["boom"] = "nil map write" // panics
		return thingResult{}, nil, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{})
	if !IsCode(err, CodeInternal) {
		t.Fatalf("err = %v, want INTERNAL", err)
	}
	var se *Error
	if !errors.As(err, &se) {
		t.Fatal("err is not a SystemError")
	}
	if se.Cause == nil {
		t.Fatal("the panic value must survive as the cause")
	}
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	k, _, _ := newTestKernel(t)
	h := func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		return thingResult{}, nil, nil
	}
	if err := Register(k, h); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := Register(k, h); !IsCode(err, CodeConflict) {
		t.Fatalf("second Register err = %v, want CONFLICT", err)
	}
}

func TestRegisterPanicsOnNilHandler(t *testing.T) {
	k, _, _ := newTestKernel(t)
	defer func() {
		if recover() == nil {
			t.Fatal("Register(nil) must panic")
		}
	}()
	_ = Register[createThing, thingResult](k, nil)
}

func TestOwnerLevelCommandsKeyOnOwner(t *testing.T) {
	if got := serializerKey(testPrincipal(), Target{}); got != "owner/u1" {
		t.Fatalf("owner-level key = %q, want owner/u1", got)
	}
	if got := serializerKey(testPrincipal(), Target{Kind: "conversation", ID: "c1"}); got != "conversation/c1" {
		t.Fatalf("resource key = %q, want conversation/c1", got)
	}
}

func TestDefaultChainOrder(t *testing.T) {
	var order []string
	k, _, _ := newTestKernel(t, WithMiddleware(func(next Next) Next {
		return func(ctx context.Context, env Envelope) (any, error) {
			order = append(order, "user")
			return next(ctx, env)
		}
	}))
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		order = append(order, "handler")
		return thingResult{}, nil, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// recovery, logging, validation, then the host's middleware, then the
	// handler. Validation is observable only by its absence from the order
	// slice, so the first entry after the defaults is the host's.
	if len(order) != 2 || order[0] != "user" || order[1] != "handler" {
		t.Fatalf("order = %v, want [user handler]", order)
	}
}

// anyCommand is an interface-typed command, so a caller can dispatch a nil
// one and reach the validation guard.
type anyCommand interface {
	CommandName() string
}

func TestDispatchRejectsNilCommand(t *testing.T) {
	k, _, _ := newTestKernel(t)
	_, err := Dispatch[anyCommand, thingResult](context.Background(), k, testPrincipal(), nil)
	if !IsCode(err, CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
}

func TestIdempotentResultMustBeSerializable(t *testing.T) {
	k, _, _ := newTestKernel(t)
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (chan int, []Event, error) {
		return make(chan int), nil, nil // a channel is not JSON-serializable
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := Dispatch[createThing, chan int](context.Background(), k, testPrincipal(), createThing{}, WithIdempotencyKey("k3"))
	if !IsCode(err, CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
	if !strings.Contains(err.Error(), "serializable") {
		t.Fatalf("message should say why: %v", err)
	}
}

func TestReplayResultThatDoesNotDecodeIsInternal(t *testing.T) {
	k, _, st := newTestKernel(t)
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		t.Error("handler must not run on a replay")
		return thingResult{}, nil, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Store a receipt whose result cannot decode into thingResult.
	repos, err := st.Scoped(store.Scope{Owner: "u1"})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	if _, created, err := repos.Receipts().Reserve(context.Background(), "k4", time.Hour); err != nil || !created {
		t.Fatalf("Reserve = created %v err %v", created, err)
	}
	if err := repos.Receipts().Complete(context.Background(), "k4", json.RawMessage(`[1,2,3]`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	_, err = Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{}, WithIdempotencyKey("k4"))
	if !IsCode(err, CodeInternal) {
		t.Fatalf("err = %v, want INTERNAL", err)
	}
}

// emptyNameCommand has an empty name, which Register must refuse.
type emptyNameCommand struct{}

func (emptyNameCommand) CommandName() string { return "" }

func TestRegisterRejectsEmptyCommandName(t *testing.T) {
	k, _, _ := newTestKernel(t)
	err := Register(k, func(ctx context.Context, tx store.Tx, c emptyNameCommand) (thingResult, []Event, error) {
		return thingResult{}, nil, nil
	})
	if !IsCode(err, CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
}

func TestPanicValueError(t *testing.T) {
	p := panicValue{v: "kaboom"}
	if got := p.Error(); got != "panic: kaboom" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestValidateRejectsNamelessEnvelope(t *testing.T) {
	var got error
	next := func(ctx context.Context, env Envelope) (any, error) { return nil, nil }
	_, err := validate(next)(context.Background(), Envelope{Command: createThing{}})
	if !IsCode(err, CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
	got = err
	if got == nil {
		t.Fatal("validate must reject")
	}
}

// Compensation for external effects is a per-handler concern: the handler
// receives tx and registers AfterCommit/AfterRollback on it. The kernel owns
// no hook type, because spec section 7's type list defines none — but the
// pattern must actually work through Dispatch, which is what this proves.
func TestHandlerCompensationRunsOnRollback(t *testing.T) {
	k, _, _ := newTestKernel(t)
	boom := errors.New("downstream failed")
	var calls []string

	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		// An external effect the transaction cannot undo by itself.
		calls = append(calls, "external-effect")
		tx.AfterRollback(func() { calls = append(calls, "refund") })
		tx.AfterCommit(func() { calls = append(calls, "confirm") })
		return thingResult{}, []Event{thingCreated{}}, boom
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if strings.Join(calls, ",") != "external-effect,refund" {
		t.Fatalf("calls = %v, want the rollback hook to run and the commit hook not to", calls)
	}
}

func TestHandlerCompensationRunsOnCommit(t *testing.T) {
	k, _, _ := newTestKernel(t)
	var calls []string

	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		calls = append(calls, "external-effect")
		tx.AfterRollback(func() { calls = append(calls, "refund") })
		tx.AfterCommit(func() { calls = append(calls, "confirm") })
		return thingResult{}, []Event{thingCreated{}}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Join(calls, ",") != "external-effect,confirm" {
		t.Fatalf("calls = %v, want the commit hook to run and the rollback hook not to", calls)
	}
}

// A compensation hook that runs on a rolled-back idempotent command must not
// be re-run when the same key is retried and succeeds.
func TestCompensationDoesNotDuplicateAcrossReplay(t *testing.T) {
	k, _, _ := newTestKernel(t)
	var calls []string
	fail := errors.New("first attempt fails")

	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		calls = append(calls, "effect")
		tx.AfterRollback(func() { calls = append(calls, "refund") })
		tx.AfterCommit(func() { calls = append(calls, "confirm") })
		if len(calls) == 1 {
			return thingResult{}, nil, fail // first attempt: rollback + refund
		}
		return thingResult{ID: "t1"}, []Event{thingCreated{}}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx := context.Background()
	if _, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{}, WithIdempotencyKey("k5")); !errors.Is(err, fail) {
		t.Fatalf("first err = %v, want fail", err)
	}
	if _, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{}, WithIdempotencyKey("k5")); err != nil {
		t.Fatalf("retry err = %v", err)
	}
	if strings.Join(calls, ",") != "effect,refund,effect,confirm" {
		t.Fatalf("calls = %v, want the retry to re-run the effect and confirm it", calls)
	}
}
