package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// Register installs the handler for a command's name, derived from C. It
// returns an error for a duplicate name, and panics for a nil handler, which
// is a programmer mistake rather than a runtime condition.
//
// C's CommandName must be safe on the zero value — define it with a value
// receiver on a struct — because the name is read from a zero C.
func Register[C Command, R any](k *Kernel, h Handler[C, R]) error {
	if h == nil {
		panic("kernel: Register called with a nil handler")
	}
	var zero C
	name := zero.CommandName()
	if name == "" {
		return NewError(CodeInvalid, "command has an empty name")
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if _, dup := k.handlers[name]; dup {
		return NewError(CodeConflict, "a handler is already registered for "+name).
			WithOperation(name)
	}
	k.handlers[name] = func(ctx context.Context, tx store.Tx, env Envelope) (any, []Event, error) {
		cmd, ok := env.Command.(C)
		if !ok {
			return nil, nil, NewError(CodeInternal, "command does not match its registered type").
				WithOperation(name)
		}
		return h(ctx, tx, cmd)
	}
	return nil
}

// replayedResult is the terminal's way of saying "this result came from a
// stored idempotency receipt, not from the handler". Dispatch decodes it,
// because only Dispatch knows R.
type replayedResult struct{ raw json.RawMessage }

// errReplayed short-circuits store.Atomic when an idempotent command finds
// its result already stored. Returning it rolls the transaction back, so the
// reservation this attempt made is discarded and no events are published.
var errReplayed = errors.New("kernel: idempotent result already stored")

// DispatchAny runs one command through the whole pipeline and returns the
// handler's result without a compile-time result type. It is the escape
// hatch for heterogenous callers — an effects dispatcher running commands a
// model's blocks declared — that cannot name the result type. Idempotent
// replays decode to generic JSON.
func DispatchAny(ctx context.Context, k *Kernel, p Principal, c Command, opts ...DispatchOption) (any, error) {
	env, out, err := beginDispatch(ctx, k, p, c, opts)
	if err != nil {
		return nil, err
	}
	if rr, ok := out.(replayedResult); ok {
		var v any
		if err := json.Unmarshal(rr.raw, &v); err != nil {
			return nil, WrapError(CodeInternal, "stored idempotent result does not decode", err).
				WithOperation(env.Name)
		}
		return v, nil
	}
	return out, nil
}

// Dispatch runs one command through the whole pipeline and returns its
// result. Every failure is a *common.SystemError carrying one of the kernel's
// eight codes; the cause stays on the chain, so errors.Is still reaches a
// store sentinel or a context error.
func Dispatch[C Command, R any](ctx context.Context, k *Kernel, p Principal, c C, opts ...DispatchOption) (R, error) {
	var zero R
	env, out, err := beginDispatch(ctx, k, p, c, opts)
	if err != nil {
		return zero, err
	}
	if rr, ok := out.(replayedResult); ok {
		var r R
		if err := json.Unmarshal(rr.raw, &r); err != nil {
			return zero, WrapError(CodeInternal, "stored idempotent result does not decode for this command", err).
				WithOperation(env.Name)
		}
		return r, nil
	}
	r, ok := out.(R)
	if !ok {
		return zero, NewError(CodeInternal, "handler returned an unexpected result type").
			WithOperation(env.Name)
	}
	return r, nil
}

// beginDispatch is the shared head of Dispatch and DispatchAny: the guard
// checks, option application and envelope construction, then the chain call.
func beginDispatch(ctx context.Context, k *Kernel, p Principal, c Command, opts []DispatchOption) (Envelope, any, error) {
	if k == nil {
		return Envelope{}, nil, NewError(CodeInternal, "nil kernel")
	}
	if err := ctx.Err(); err != nil {
		return Envelope{}, nil, wrapContext(err)
	}
	// A nil interface must be rejected before CommandName is called on it.
	// any(c) is nil only when C is an interface type holding nil; a struct
	// command boxes to a non-nil interface.
	if any(c) == nil {
		return Envelope{}, nil, NewError(CodeInvalid, "nil command")
	}

	dc := dispatchConfig{}
	for _, o := range opts {
		o(&dc)
	}

	env := Envelope{
		ID:             k.ids.NewID(),
		Name:           c.CommandName(),
		Principal:      p,
		At:             k.clock.Now(),
		CorrelationID:  dc.correlation,
		IdempotencyKey: dc.idempotencyKey,
		Command:        c,
	}
	out, err := k.chain(ctx, env)
	return env, out, err
}

// buildChain composes the middleware. The defaults are applied outermost, so
// they run on every command regardless of what a host adds: recovery, then
// logging, then validation, matching the order the specification gives.
func (k *Kernel) buildChain(m []Middleware) Next {
	var next Next = k.execute
	// Applying in reverse makes m[0] the outermost of the host's middleware.
	for i := len(m) - 1; i >= 0; i-- {
		next = m[i](next)
	}
	next = validate(next)
	next = k.logMiddleware(next)
	next = recovery(next)
	return next
}

// recovery converts a panic into an Internal error rather than letting it
// take down the host process: one bad command should not kill the workspace.
func recovery(next Next) Next {
	return func(ctx context.Context, env Envelope) (out any, err error) {
		defer func() {
			if r := recover(); r != nil {
				out = nil
				err = WrapError(CodeInternal, "command panicked", panicValue{v: r, stack: debug.Stack()}).
					WithOperation(env.Name)
			}
		}()
		return next(ctx, env)
	}
}

// panicValue is the cause a recovered panic carries, so the panic value and
// the stack at the time survive into the error a host logs.
type panicValue struct {
	v     any
	stack []byte
}

func (p panicValue) Error() string { return fmt.Sprintf("panic: %v", p.v) }

// logMiddleware records the outcome of every command. It logs the command
// name, the principal's owner, the duration and the error code — never the
// payload, which the section 15 security rules forbid.
func (k *Kernel) logMiddleware(next Next) Next {
	return func(ctx context.Context, env Envelope) (any, error) {
		start := k.clock.Now()
		out, err := next(ctx, env)
		dur := k.clock.Now().Sub(start)
		attrs := []slog.Attr{
			slog.String("command", env.Name),
			slog.String("owner", env.Principal.Owner),
			slog.String("correlation_id", env.CorrelationID),
			slog.Duration("duration", dur),
		}
		if err != nil {
			attrs = append(attrs, slog.String("code", string(CodeOf(err))))
			k.logger.LogAttrs(ctx, slog.LevelWarn, "command failed", attrs...)
		} else {
			k.logger.LogAttrs(ctx, slog.LevelDebug, "command succeeded", attrs...)
		}
		return out, err
	}
}

// validate rejects an envelope that cannot possibly be a legitimate command,
// before the authorizer or the store is involved.
func validate(next Next) Next {
	return func(ctx context.Context, env Envelope) (any, error) {
		if env.Command == nil {
			return nil, NewError(CodeInvalid, "nil command").WithOperation(env.Name)
		}
		if env.Name == "" {
			return nil, NewError(CodeInvalid, "command has no name")
		}
		return next(ctx, env)
	}
}

// execute is the terminal: authorize, admit through the serializer, run the
// handler inside store.Atomic reserving the idempotency key first, then
// publish the handler's events only once the commit has succeeded.
func (k *Kernel) execute(ctx context.Context, env Envelope) (any, error) {
	t := targetOf(env.Command)

	if err := k.authorizer.Authorize(ctx, env.Principal, env.Name, t); err != nil {
		return nil, classifyAuthz(err, env.Name, t)
	}

	k.mu.RLock()
	h, ok := k.handlers[env.Name]
	k.mu.RUnlock()
	if !ok {
		return nil, NewError(CodeNotFound, "no handler registered for "+env.Name).
			WithOperation(env.Name).
			WithPath(t.ID)
	}

	// The serializer is released before the caller reaches a provider: the
	// transaction is closed by the time this returns.
	key := serializerKey(env.Principal, t)
	if err := k.ser.lock(ctx, key); err != nil {
		return nil, err
	}
	defer k.ser.unlock(key)

	var (
		result any
		events []Event
		replay json.RawMessage
	)

	err := k.store.Atomic(ctx, store.Scope{Owner: env.Principal.Owner}, store.TxOptions{Timeout: k.txTimeout}, func(ctx context.Context, tx store.Tx) error {
		if env.IdempotencyKey != "" {
			rec, created, err := tx.Receipts().Reserve(ctx, env.IdempotencyKey, k.receiptTTL)
			if err != nil {
				return err
			}
			if !created {
				if rec.State == domain.ReceiptCompleted {
					replay = rec.Result
					return errReplayed
				}
				return NewError(CodeConflict, "an identical command is already in flight").
					WithOperation(env.Name).
					WithPath(t.ID).
					WithRetryable(true)
			}
		}

		var herr error
		result, events, herr = h(ctx, tx, env)
		if herr != nil {
			return herr
		}

		if env.IdempotencyKey != "" {
			raw, err := json.Marshal(result)
			if err != nil {
				return WrapError(CodeInvalid, "result is not JSON-serializable; an idempotent command must return a serializable result", err).
					WithOperation(env.Name)
			}
			if err := tx.Receipts().Complete(ctx, env.IdempotencyKey, raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errReplayed) {
			return replayedResult{raw: replay}, nil
		}
		return nil, wrapStore(wrapContext(err))
	}

	k.publish(env, events)
	return result, nil
}

// publish turns the handler's events into envelopes and offers them to the
// bus. It runs after the commit, so a rolled-back command publishes nothing.
func (k *Kernel) publish(env Envelope, events []Event) {
	if len(events) == 0 {
		return
	}
	convID := conversationOf(env.Command)
	for _, e := range events {
		k.bus.publish(EventEnvelope{
			Name:           e.EventName(),
			Owner:          env.Principal.Owner,
			ConversationID: convID,
			CorrelationID:  env.CorrelationID,
			At:             k.clock.Now(),
			Event:          e,
		})
	}
}

// classifyAuthz forces an authorizer's refusal into Forbidden unless it
// already carries a code, so the default is deny and the reason survives.
func classifyAuthz(err error, name string, t Target) error {
	if se, ok := err.(*Error); ok && se.Code != "" {
		return err
	}
	return WrapError(CodeForbidden, "not authorized", err).
		WithOperation(name).
		WithPath(t.ID)
}
