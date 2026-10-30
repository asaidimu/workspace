package kernel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/memory"
)

func TestSystemClockIsTheOnlyTimeNow(t *testing.T) {
	got := systemClock{}.Now()
	if got.IsZero() {
		t.Fatal("the default clock returned the zero time")
	}
	if d := time.Since(got); d < -time.Minute || d > time.Minute {
		t.Fatalf("the default clock is not close to wall time: %v", d)
	}
}

func TestUUIDIDsAreV7(t *testing.T) {
	id := uuidIDs{}.NewID()
	if len(id) != 36 {
		t.Fatalf("id %q is not a UUID string", id)
	}
	// A v7 UUID starts with the low 48 bits of a millisecond timestamp, so
	// it sorts by creation time; the version nibble is the 13th character.
	if id[14] != '7' {
		t.Fatalf("id %q is not a UUIDv7", id)
	}
}

func TestOptionsAreApplied(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)}
	st := memory.New(memory.Options{Clock: clock})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var sawMiddleware bool

	k, err := New(st,
		WithAuthorizer(AllowOwner()),
		WithClock(clock),
		WithIDs(&seqIDs{}),
		WithLogger(logger),
		WithReceiptTTL(time.Hour),
		WithMiddleware(func(next Next) Next {
			return func(ctx context.Context, env Envelope) (any, error) {
				sawMiddleware = true
				return next(ctx, env)
			}
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if k.receiptTTL != time.Hour {
		t.Fatalf("receiptTTL = %v, want 1h", k.receiptTTL)
	}
	if k.logger != logger {
		t.Fatal("WithLogger did not take effect")
	}

	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		return thingResult{}, nil, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !sawMiddleware {
		t.Fatal("the host middleware never ran")
	}
}

func TestNewRejectsNonPositiveReceiptTTL(t *testing.T) {
	st := memory.New(memory.Options{})
	_, err := New(st,
		WithAuthorizer(AllowOwner()),
		WithClock(&fakeClock{}),
		WithIDs(&seqIDs{}),
		WithReceiptTTL(0),
	)
	if !IsCode(err, CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
}

func TestNewRejectsNilClockOrIDs(t *testing.T) {
	st := memory.New(memory.Options{})
	_, err := New(st, WithAuthorizer(AllowOwner()), WithIDs(&seqIDs{}), WithClock(nil))
	if !IsCode(err, CodeInvalid) {
		t.Fatalf("nil clock err = %v, want INVALID", err)
	}
	_, err = New(st, WithAuthorizer(AllowOwner()), WithClock(&fakeClock{}), WithIDs(nil))
	if !IsCode(err, CodeInvalid) {
		t.Fatalf("nil ids err = %v, want INVALID", err)
	}
}

// fixedAuthorizer refuses with an already-coded error, so classifyAuthz must
// pass it through instead of re-wrapping it in Forbidden.
type fixedAuthorizer struct{ err error }

func (a fixedAuthorizer) Authorize(ctx context.Context, p Principal, name string, t Target) error {
	return a.err
}

func TestClassifyAuthzPreservesCodedErrors(t *testing.T) {
	coded := NewError(CodeNotFound, "no such scope").WithOperation("thing.create")
	k, _, _ := newTestKernel(t, WithAuthorizer(fixedAuthorizer{err: coded}))
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		t.Error("handler must not run")
		return thingResult{}, nil, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := Dispatch[createThing, thingResult](context.Background(), k, testPrincipal(), createThing{})
	if !IsCode(err, CodeNotFound) {
		t.Fatalf("code = %q, want the authorizer's NOT_FOUND, not FORBIDDEN", CodeOf(err))
	}
	if !strings.Contains(err.Error(), "no such scope") {
		t.Fatalf("the authorizer's message was lost: %v", err)
	}
}

func TestClassifyAuthzDefaultsToForbidden(t *testing.T) {
	raw := errors.New("denied")
	err := classifyAuthz(raw, "thing.create", Target{ID: "conv-1"})
	if !IsCode(err, CodeForbidden) {
		t.Fatalf("code = %q, want FORBIDDEN", CodeOf(err))
	}
	if !errors.Is(err, raw) {
		t.Fatal("the authorizer's error must stay on the chain")
	}
}

func TestDispatchOnNilKernel(t *testing.T) {
	if _, err := Dispatch[createThing, thingResult](context.Background(), nil, testPrincipal(), createThing{}); !IsCode(err, CodeInternal) {
		t.Fatalf("err = %v, want INTERNAL", err)
	}
}

func TestDispatchWithCancelledContext(t *testing.T) {
	k, _, _ := newTestKernel(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{}); !IsCode(err, CodeCanceled) {
		t.Fatalf("err = %v, want CANCELED", err)
	}
}

func TestDispatchPropagatesCorrelationID(t *testing.T) {
	k, _, _ := newTestKernel(t)
	if err := Register(k, func(ctx context.Context, tx store.Tx, c createThing) (thingResult, []Event, error) {
		return thingResult{}, []Event{thingCreated{ID: "t1"}}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := k.Bus().Subscribe(ctx, EventFilter{}, 8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := Dispatch[createThing, thingResult](ctx, k, testPrincipal(), createThing{}, WithCorrelation("corr-9")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if env := <-sub; env.CorrelationID != "corr-9" {
		t.Fatalf("correlation = %q, want corr-9", env.CorrelationID)
	}
}

func TestBusReturnsTheKernelBus(t *testing.T) {
	k, _, _ := newTestKernel(t)
	if k.Bus() == nil {
		t.Fatal("Bus() must not be nil")
	}
	if k.Bus() != k.Bus() {
		t.Fatal("Bus() must return the same bus")
	}
}
