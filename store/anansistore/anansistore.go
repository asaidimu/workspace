// Package anansistore is the go-anansi adapter for the store port.
//
// It is the only package allowed to import the anansi persistence stack
// (enforced by boundary/boundary_test.go). The design, verified against the
// spikes in docs/spike.md and recorded in todo/done/anansistore.md:
//
//   - Ephemeral is out; the adapter runs on any base.Persistence the caller
//     wires (tests use SQLite in-memory).
//   - anansi mints _id_ (caller-chosen ids are rejected unless 32-hex), so
//     domain identity lives in unique scalar fields (conversation_id,
//     turn_key = id@version, content_key = key/version, ...).
//   - _metadata_.version is the domain Rev: create starts at 1, a stale
//     CollectionUpdate.Version reports count=0 err=nil and is translated to
//     ErrConflict here, with a pre-read inside the same transaction to
//     distinguish "missing" (ErrNotFound) from "stale" (ErrConflict).
//   - Atomic binds fn to p.Transact; tx repos capture the transaction
//     context (the context IS the transaction carrier) and scoped writes wrap
//     their read-modify-write in a mini Transact that joins flat when a port
//     transaction is already open.
//   - Free-form JSON (blocks, usage, state, ...) is stored as opaque string
//     fields so exact numbers survive; topics/heartbeat and anything not a
//     scalar are filtered residually in Go, mirroring store/memory.
package anansistore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
	"go.uber.org/zap"
)

// Options configures an anansistore store.
type Options struct {
	// Prefix is prepended to every collection name; defaults to "ws_".
	Prefix string
	// Clock is the injected time source; nil means time.Now.
	Clock domain.Clock
	// IDs is accepted for port symmetry with the spec (Options: Clock,
	// IDGenerator). anansi mints document ids itself, so the adapter does
	// not use it; domain ids always come from the caller.
	IDs domain.IDGenerator
	// Logger is adapter-side diagnostics only; the persistence's own logger
	// is owned by whoever wired it.
	Logger *zap.Logger
}

// Store implements store.Store on top of a go-anansi base.Persistence. The
// caller owns the persistence (and any *sql.DB behind it): Close does not
// shut anything down.
type Store struct {
	p      base.Persistence
	prefix string
	now    func() time.Time
	logger *zap.Logger
	cols   map[string]base.Collection
}

// docFactoryOnce guards the process-wide anansi document factory; it is a
// singleton and must be configured before any persistence op mints
// documents, otherwise anansi panics with ERR_DATA_FACTORY_NOT_CONFIGURED.
var docFactoryOnce sync.Once

// errReadOnlyRollback forces p.Transact to roll back a read-only Atomic whose
// fn succeeded: anansi has no read-only transaction flag, so we return a
// sentinel the adapter swallows below while hooks still see a clean success.
var errReadOnlyRollback = errors.New("anansistore: read-only rollback")

// New builds a store over p, creating the nine spec collections (idempotent)
// from struct-derived schemas.
func New(p base.Persistence, opts Options) (store.Store, error) {
	if p == nil {
		return nil, errors.New("anansistore: persistence is nil")
	}
	if opts.Prefix == "" {
		opts.Prefix = "ws_"
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	now := time.Now
	if opts.Clock != nil {
		now = opts.Clock.Now
	}
	docFactoryOnce.Do(func() {
		if err := data.ConfigureDocumentFactory(data.DocumentFactoryConfig{}, logger); err != nil {
			logger.Warn("anansistore: document factory configuration", zap.Error(err))
		}
	})

	ctx := context.Background()
	s := &Store{p: p, prefix: opts.Prefix, now: now, logger: logger, cols: make(map[string]base.Collection, len(collectionSpecs))}
	for _, spec := range collectionSpecs {
		if err := s.ensureCollection(ctx, spec); err != nil {
			return nil, err
		}
		coll, err := p.Collection(ctx, opts.Prefix+spec.suffix)
		if err != nil {
			return nil, fmt.Errorf("anansistore: open collection %s%s: %w", opts.Prefix, spec.suffix, translate(err))
		}
		s.cols[spec.suffix] = coll
	}
	return s, nil
}

// ensureCollection derives the schema and creates the collection if missing.
func (s *Store) ensureCollection(ctx context.Context, spec collectionSpec) error {
	has, err := s.p.HasCollection(ctx, s.prefix+spec.suffix)
	if err != nil {
		return fmt.Errorf("anansistore: has collection %s%s: %w", s.prefix, spec.suffix, translate(err))
	}
	if has {
		return nil
	}
	sch, err := deriveSchema(s.prefix, spec)
	if err != nil {
		return err
	}
	if _, err := s.p.CreateCollection(ctx, sch); err != nil {
		// A concurrent init may have won the race; accept "already exists".
		if alreadyExistsCollection(err) {
			return nil
		}
		return fmt.Errorf("anansistore: create collection %s: %w", sch.Name, translate(err))
	}
	return nil
}

// col returns a collection handle by spec suffix.
func (s *Store) col(suffix string) base.Collection { return s.cols[suffix] }

func validScope(scope store.Scope) error {
	if scope.System {
		return nil
	}
	if scope.Owner == "" { // memory trims; empty owner invalid either way
		return store.ErrInvalid
	}
	return nil
}

// Scoped returns a session over the store for one owner (or System).
func (s *Store) Scoped(scope store.Scope) (store.Repos, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	return &repos{st: s, scope: scope}, nil
}

// Atomic runs fn inside a go-anansi transaction. The context fn receives is
// the transaction context: every anansi call made with it joins the same
// transaction (the kernel's dispatch passes it through to handlers).
func (s *Store) Atomic(ctx context.Context, scope store.Scope, opts store.TxOptions, fn func(ctx context.Context, tx store.Tx) error) error {
	if err := validScope(scope); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	tx := &repos{st: s, scope: scope, txState: &txState{}}
	var fnErr error
	_, err := s.p.Transact(ctx, func(tctx context.Context, _ base.BasePersistence) (any, error) {
		tx.txState.tctx = tctx
		fnErr = fn(tctx, tx)
		if opts.ReadOnly {
			if fnErr != nil {
				return nil, fnErr
			}
			return nil, errReadOnlyRollback
		}
		return nil, fnErr
	})
	tx.txState.closed = true

	if opts.ReadOnly {
		if fnErr != nil {
			tx.runRollback()
			return fnErr
		}
		if errors.Is(err, errReadOnlyRollback) {
			tx.runCommit()
			return nil
		}
		tx.runRollback()
		return translate(err)
	}
	if fnErr != nil {
		tx.runRollback()
		return fnErr
	}
	if err != nil {
		tx.runRollback()
		return translate(err)
	}
	tx.runCommit()
	return nil
}

// Maintenance returns the garbage-collection surface. Like the memory
// adapter, the sweeps are no-ops: real collection is the blob reconcile job
// and the fault-injection phase's sweeps (recorded deviation, parity with
// store/memory so the conformance suite and facade behave identically).
func (s *Store) Maintenance(scope store.Scope) (store.Maintenance, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	return &maintenance{scope: scope}, nil
}

// Migrator re-ensures the collection set (New already did it once) and
// reports the schema version.
func (s *Store) Migrator() store.Migrator { return &migrator{st: s} }

// Capabilities reports what this adapter can do: transactions are
// serializable (sqlite single-writer + busy-retry, and port transactions
// serialize through the connection pool), the chain walk is iterative so
// RecursiveQueries is false, and there is no configured value ceiling.
func (s *Store) Capabilities(ctx context.Context) (store.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return store.Capabilities{}, err
	}
	return store.Capabilities{
		Transactions:      store.TxSerializable,
		RecursiveQueries:  false,
		AtomicIncrement:   true,
		ConcurrentWriters: true,
		MaxValueBytes:     0,
	}, nil
}

// Close is a no-op: the caller owns the persistence lifecycle. It only
// mirrors the port contract (context cancellation surfaces as an error).
func (s *Store) Close(ctx context.Context) error { return ctx.Err() }

// --- transaction and scoped plumbing ---

// txState is the per-Atomic state shared by every repo handed out by one
// transaction.
type txState struct {
	tctx          context.Context
	closed        bool
	afterCommit   []func()
	afterRollback []func()
}

// repos implements store.Repos for both modes: scoped (txState nil, every
// write wrapped in its own mini transaction) and transactional (tctx carries
// the open transaction).
type repos struct {
	st      *Store
	scope   store.Scope
	txState *txState
}

// AfterCommit registers a hook to run when the port transaction commits;
// hooks run exactly once, at the end of Atomic, mirroring store/memory.
func (r *repos) AfterCommit(hook func()) {
	if hook == nil || r.txState == nil {
		return
	}
	r.txState.afterCommit = append(r.txState.afterCommit, hook)
}

// AfterRollback registers a hook to run when the port transaction rolls back.
func (r *repos) AfterRollback(hook func()) {
	if hook == nil || r.txState == nil {
		return
	}
	r.txState.afterRollback = append(r.txState.afterRollback, hook)
}

func (r *repos) runCommit() {
	if r.txState == nil {
		return
	}
	r.txState.closed = true
	for _, h := range r.txState.afterCommit {
		h()
	}
}

func (r *repos) runRollback() {
	if r.txState == nil {
		return
	}
	r.txState.closed = true
	for _, h := range r.txState.afterRollback {
		h()
	}
}

func (r *repos) Conversations() store.ConversationRepo { return &conversationRepo{r: r} }
func (r *repos) Turns() store.TurnRepo                 { return &turnRepo{r: r} }
func (r *repos) Contents() store.ContentRepo           { return &contentRepo{r: r} }
func (r *repos) Roles() store.RoleRepo                 { return &roleRepo{r: r} }
func (r *repos) Blobs() store.BlobRepo                 { return &blobRepo{r: r} }
func (r *repos) Runs() store.RunRepo                   { return &runRepo{r: r} }
func (r *repos) Receipts() store.ReceiptRepo           { return &receiptRepo{r: r} }
func (r *repos) Settings() store.SettingsRepo          { return &settingsRepo{r: r} }

// rctx resolves the context for a read (or a plain single-op write):
// transactional repos always use the transaction context — context is the
// transaction carrier — while scoped repos use the caller's context. A closed
// transaction reports ErrTxClosed before anything else.
func (r *repos) rctx(cctx context.Context) (context.Context, error) {
	if r.txState != nil {
		if r.txState.closed {
			return nil, store.ErrTxClosed
		}
		return r.txState.tctx, nil
	}
	return cctx, nil
}

// write runs fn with a context bound to a transaction. Inside an open port
// transaction it runs directly on tctx; a scoped write gets its own mini
// transaction so its read-modify-write sequence is atomic. Nested Transact
// joins flat, so a scoped repo used from inside Atomic still joins the open
// transaction instead of deadlocking.
func (r *repos) write(cctx context.Context, fn func(wctx context.Context) error) error {
	if r.txState != nil {
		if r.txState.closed {
			return store.ErrTxClosed
		}
		return fn(r.txState.tctx)
	}
	_, err := r.st.p.Transact(cctx, func(wctx context.Context, _ base.BasePersistence) (any, error) {
		return nil, fn(wctx)
	})
	return translate(err)
}

// ready resolves the context for an op. Scoped ops use the caller's context
// unchecked (store/memory only checks ctx.Err in Create/Append, and this
// mirrors it); transactional ops must see a live transaction.
func (r *repos) ready(cctx context.Context) (context.Context, error) {
	return r.rctx(cctx)
}
