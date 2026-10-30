package kernel

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asaidimu/workspace/store"
)

// Kernel is the command dispatcher. It owns the middleware chain, the
// authorizer, the keyed serializer and the event bus.
type Kernel struct {
	store      store.Store
	authorizer Authorizer
	clock      Clock
	ids        IDGenerator
	logger     *slog.Logger
	receiptTTL time.Duration
	txTimeout  time.Duration
	bus        *bus
	ser        *serializer
	chain      Next

	// handlers maps a command name to its type-erased handler. Registration
	// happens before the workspace is handed to a caller, so reads take the
	// read lock and only Register takes the write lock.
	handlers map[string]erasedHandler
	mu       sync.RWMutex
}

// erasedHandler is a Handler[C, R] with its type parameters removed, so a
// dispatch can look a handler up by name without knowing C or R. The wrapper
// installed by Register does the assertion back to C.
type erasedHandler func(ctx context.Context, tx store.Tx, env Envelope) (any, []Event, error)

// config is the immutable result of applying Options at construction.
type config struct {
	authorizer Authorizer
	clock      Clock
	ids        IDGenerator
	middleware []Middleware
	logger     *slog.Logger
	receiptTTL time.Duration
	txTimeout  time.Duration
}

// dispatchConfig is the per-dispatch settings.
type dispatchConfig struct {
	idempotencyKey string
	correlation    string
}

// Option configures a Kernel at construction.
type Option func(*config)

// DispatchOption configures one Dispatch call.
type DispatchOption func(*dispatchConfig)

// defaultReceiptTTL bounds how long a reserved idempotency key blocks a
// retry. It is a kernel default rather than a spec-pinned one: the section 15
// table does not list a receipt TTL, so a host that needs a different window
// sets it here.
const defaultReceiptTTL = 24 * time.Hour

// WithMiddleware appends to the middleware chain, outermost first. The
// kernel's own chain is recovery, then logging, then validation, so user
// middleware runs inside those and closest to the handler.
func WithMiddleware(m ...Middleware) Option {
	return func(c *config) { c.middleware = append(c.middleware, m...) }
}

// WithAuthorizer replaces the authorizer. The default is deny-all, so a
// workspace without one cannot be opened.
func WithAuthorizer(a Authorizer) Option {
	return func(c *config) { c.authorizer = a }
}

// WithClock injects the time source. Nothing in the core calls time.Now
// except the default clock, which exists so a host that does not care can
// still open a kernel.
func WithClock(c Clock) Option {
	return func(cfg *config) { cfg.clock = c }
}

// WithIDs injects the UUIDv7 generator.
func WithIDs(g IDGenerator) Option {
	return func(c *config) { c.ids = g }
}

// WithLogger injects the structured logger. Command names, owners, durations
// and error codes are logged; payloads never are (section 15, security).
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithReceiptTTL sets how long a reserved idempotency key stays reserved. A
// retry that finds an unexpired reservation and no stored result is told the
// original is still in flight rather than being run twice.
// WithTxTimeout bounds every command's store transaction (section 15's
// TxTimeout). Zero leaves the store's own default in place.
func WithTxTimeout(d time.Duration) Option {
	return func(c *config) { c.txTimeout = d }
}

func WithReceiptTTL(d time.Duration) Option {
	return func(c *config) { c.receiptTTL = d }
}

// WithIdempotencyKey makes a dispatch idempotent: a repeated key returns the
// stored result, so results of idempotent commands must be
// JSON-serializable.
func WithIdempotencyKey(key string) DispatchOption {
	return func(c *dispatchConfig) { c.idempotencyKey = key }
}

// WithCorrelation tags the dispatch and its events with a correlation ID.
func WithCorrelation(id string) DispatchOption {
	return func(c *dispatchConfig) { c.correlation = id }
}

// Bus returns the kernel's event bus.
func (k *Kernel) Bus() Bus { return k.bus }

// Emit publishes an event from inside the workspace itself, for a producer
// that is not a command handler: the agent loop commits through the run
// repository rather than through a command, so dispatch has nothing to
// publish for it. Like dispatch's own publish, Emit must be called only
// after the state the event reports is committed — a rolled-back write must
// not announce itself. Hosts do not call Emit; they Subscribe.
func (k *Kernel) Emit(e Event, owner, conversationID string) {
	if e == nil {
		return
	}
	k.bus.publish(EventEnvelope{
		Name:           e.EventName(),
		Owner:          owner,
		ConversationID: conversationID,
		At:             k.clock.Now(),
		Event:          e,
	})
}

// systemClock is the default time source. It is the only place in the core
// that calls time.Now, and it exists purely so a host that does not inject a
// clock still gets sensible timestamps; tests inject workspacetest.FakeClock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// uuidIDs is the default UUIDv7 generator. Tests inject workspacetest.SeqIDs.
type uuidIDs struct{}

func (uuidIDs) NewID() string { return uuid.Must(uuid.NewV7()).String() }

// DefaultClock and DefaultIDs are the production defaults New itself uses,
// exported so the facade can wire the same time source and identifier
// generator into the parts of the core that sit outside the kernel — the run
// manager and the stepper — instead of inventing a second clock.
func DefaultClock() Clock { return systemClock{} }

func DefaultIDs() IDGenerator { return uuidIDs{} }

// New builds a kernel over the given store. An authorizer is required: the
// default is deny, and a workspace that can be opened without one would be a
// workspace with no access control at all.
func New(s store.Store, opts ...Option) (*Kernel, error) {
	cfg := config{
		clock:      systemClock{},
		ids:        uuidIDs{},
		logger:     slog.Default(),
		receiptTTL: defaultReceiptTTL,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if s == nil {
		return nil, NewError(CodeInvalid, "a store is required")
	}
	if cfg.authorizer == nil {
		return nil, NewError(CodeInvalid, "an authorizer is required; use kernel.AllowOwner() for single-user apps")
	}
	if cfg.clock == nil {
		return nil, NewError(CodeInvalid, "a clock is required")
	}
	if cfg.ids == nil {
		return nil, NewError(CodeInvalid, "an ID generator is required")
	}
	if cfg.receiptTTL <= 0 {
		return nil, NewError(CodeInvalid, "the receipt TTL must be positive")
	}

	k := &Kernel{
		store:      s,
		authorizer: cfg.authorizer,
		clock:      cfg.clock,
		ids:        cfg.ids,
		logger:     cfg.logger,
		receiptTTL: cfg.receiptTTL,
		txTimeout:  cfg.txTimeout,
		bus:        newBus(),
		ser:        newSerializer(),
		handlers:   map[string]erasedHandler{},
	}
	k.chain = k.buildChain(cfg.middleware)
	return k, nil
}
