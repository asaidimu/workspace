// Package memory is the in-memory store adapter.
//
// It is the reference implementation of the store port and the baseline the
// anansistore must match. Everything lives behind one store-level mutex, so
// concurrent writers serialize and the strongest isolation level holds:
// Capabilities reports TxSerializable.
//
// Transactions stage their writes and apply them on commit. A read inside a
// transaction sees that transaction's own staged writes first (read-your-
// writes), then the committed state, and never sees another transaction's
// uncommitted work. A rollback drops the staged writes and the committed
// state is unchanged. The same logic backs Scoped, so the two are consistent.
package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// Options configures a memory store.
type Options struct {
	// Clock is the injected time source; nil means time.Now. It is used for
	// receipt expiry and blob orphan cutoffs.
	Clock domain.Clock
}

// Store is an in-memory implementation of store.Store.
type Store struct {
	mu  sync.Mutex
	now func() time.Time
	ds  *datastore
}

type datastore struct {
	conversations map[string]domain.Conversation
	turns         map[domain.TurnRef]domain.Turn
	contents      map[domain.ContentRef]domain.Content
	roles         map[string]domain.Role
	blobs         map[string]domain.Blob
	refs          map[string][]domain.EntityRef
	runs          map[string]domain.Run
	receipts      map[string]domain.Receipt
	settings      map[string]domain.Settings
}

func newStore() *datastore {
	return &datastore{
		conversations: map[string]domain.Conversation{},
		turns:         map[domain.TurnRef]domain.Turn{},
		contents:      map[domain.ContentRef]domain.Content{},
		roles:         map[string]domain.Role{},
		blobs:         map[string]domain.Blob{},
		refs:          map[string][]domain.EntityRef{},
		runs:          map[string]domain.Run{},
		receipts:      map[string]domain.Receipt{},
		settings:      map[string]domain.Settings{},
	}
}

// New returns a store backed by process memory.
func New(opts Options) *Store {
	now := time.Now
	if opts.Clock != nil {
		now = opts.Clock.Now
	}
	return &Store{now: now, ds: newStore()}
}

func (s *Store) Scoped(scope store.Scope) (store.Repos, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	return &scopedRepos{st: s, scope: scope}, nil
}

func (s *Store) Atomic(ctx context.Context, scope store.Scope, opts store.TxOptions, fn func(ctx context.Context, tx store.Tx) error) error {
	if err := validScope(scope); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	base := s.ds
	work := base.clone()
	s.mu.Unlock()

	// fn runs without the store mutex held. Transactional repos work on the
	// private clone, so they need no lock of their own, and a callback that
	// touches a scoped repo does not deadlock against a lock we no longer
	// hold. This is the point of the clone: isolation without holding the
	// mutex across arbitrary user code.
	tx := &txImpl{
		st:    s,
		scope: scope,
		ds:    work,
		base:  base,
	}
	err := fn(ctx, tx)
	if opts.ReadOnly {
		// A read-only transaction stages nothing usable: we throw the clone
		// away, so a fn that tried to write cannot persist it, and a fn that
		// only read sees the committed state. Commit hooks still run, because
		// a clean read-only call did succeed.
		if err != nil {
			tx.runAfterRollback()
			return err
		}
		tx.runAfterCommit()
		return nil
	}
	if err != nil {
		tx.runAfterRollback()
		return err
	}

	// Re-check and swap under the mutex, so a concurrent writer cannot slip
	// between the clone and the commit. Because the clone is the whole
	// datastore and the swap is a single pointer assignment, the committed
	// state never holds a half-applied transaction, which is what makes this
	// serializable rather than merely snapshot.
	s.mu.Lock()
	if s.ds != tx.base {
		s.mu.Unlock()
		tx.runAfterRollback()
		return store.ErrConflict
	}
	s.ds = work
	s.mu.Unlock()
	tx.runAfterCommit()
	return nil
}

func (s *Store) Maintenance(scope store.Scope) (store.Maintenance, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	return &maintenance{st: s, scope: scope}, nil
}

func (s *Store) Migrator() store.Migrator { return migrator{} }

func (s *Store) Capabilities(ctx context.Context) (store.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return store.Capabilities{}, err
	}
	return store.Capabilities{
		Transactions:      store.TxSerializable,
		RecursiveQueries:  true,
		AtomicIncrement:   true,
		ConcurrentWriters: true,
		MaxValueBytes:     0, // no configured ceiling
	}, nil
}

func (s *Store) Close(ctx context.Context) error { return ctx.Err() }

// clone returns a deep-enough copy that a staged transaction can mutate maps
// and slices without touching committed state.
func (d *datastore) clone() *datastore {
	n := newStore()
	for k, v := range d.conversations {
		n.conversations[k] = cloneConversation(v)
	}
	for k, v := range d.turns {
		n.turns[k] = cloneTurn(v)
	}
	for k, v := range d.contents {
		n.contents[k] = cloneContent(v)
	}
	for k, v := range d.roles {
		n.roles[k] = cloneRole(v)
	}
	for k, v := range d.blobs {
		n.blobs[k] = cloneBlob(v)
	}
	for k, v := range d.refs {
		n.refs[k] = append([]domain.EntityRef(nil), v...)
	}
	for k, v := range d.runs {
		n.runs[k] = cloneRun(v)
	}
	for k, v := range d.receipts {
		n.receipts[k] = cloneReceipt(v)
	}
	for k, v := range d.settings {
		n.settings[k] = cloneSettings(v)
	}
	return n
}

func validScope(scope store.Scope) error {
	if scope.System {
		return nil
	}
	if strings.TrimSpace(scope.Owner) == "" {
		return store.ErrInvalid
	}
	return nil
}

// --- transactions ---

type txImpl struct {
	st            *Store
	scope         store.Scope
	ds            *datastore
	base          *datastore
	closed        bool
	afterCommit   []func()
	afterRollback []func()
}

func (t *txImpl) ensureOpen() error {
	if t.closed {
		return store.ErrTxClosed
	}
	return nil
}

func (t *txImpl) AfterCommit(hook func()) {
	if hook == nil {
		return
	}
	t.afterCommit = append(t.afterCommit, hook)
}

func (t *txImpl) AfterRollback(hook func()) {
	if hook == nil {
		return
	}
	t.afterRollback = append(t.afterRollback, hook)
}

func (t *txImpl) runAfterCommit() {
	t.closed = true
	for _, h := range t.afterCommit {
		h()
	}
}

func (t *txImpl) runAfterRollback() {
	t.closed = true
	for _, h := range t.afterRollback {
		h()
	}
}

func (t *txImpl) Conversations() store.ConversationRepo {
	return &conversationRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}
func (t *txImpl) Turns() store.TurnRepo {
	return &turnRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}
func (t *txImpl) Contents() store.ContentRepo {
	return &contentRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}
func (t *txImpl) Roles() store.RoleRepo {
	return &roleRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}
func (t *txImpl) Blobs() store.BlobRepo {
	return &blobRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}
func (t *txImpl) Runs() store.RunRepo {
	return &runRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}
func (t *txImpl) Receipts() store.ReceiptRepo {
	return &receiptRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}
func (t *txImpl) Settings() store.SettingsRepo {
	return &settingsRepo{st: t.st, scope: t.scope, ds: t.ds, open: t.ensureOpen}
}

// --- scoped repos ---

// scopedRepos is a session over the store, not a snapshot of it. A
// transaction commits by swapping in a new datastore, so a handle obtained
// before the commit must keep observing committed state instead of reading
// and writing the orphaned copy it was created with; every accessor
// therefore resolves the live datastore under the store mutex.
type scopedRepos struct {
	st    *Store
	scope store.Scope
}

func (r *scopedRepos) current() *datastore {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	return r.st.ds
}

func (r *scopedRepos) Conversations() store.ConversationRepo {
	return &conversationRepo{st: r.st, scope: r.scope, ds: r.current()}
}
func (r *scopedRepos) Turns() store.TurnRepo {
	return &turnRepo{st: r.st, scope: r.scope, ds: r.current()}
}
func (r *scopedRepos) Contents() store.ContentRepo {
	return &contentRepo{st: r.st, scope: r.scope, ds: r.current()}
}
func (r *scopedRepos) Roles() store.RoleRepo {
	return &roleRepo{st: r.st, scope: r.scope, ds: r.current()}
}
func (r *scopedRepos) Blobs() store.BlobRepo {
	return &blobRepo{st: r.st, scope: r.scope, ds: r.current()}
}
func (r *scopedRepos) Runs() store.RunRepo {
	return &runRepo{st: r.st, scope: r.scope, ds: r.current()}
}
func (r *scopedRepos) Receipts() store.ReceiptRepo {
	return &receiptRepo{st: r.st, scope: r.scope, ds: r.current()}
}
func (r *scopedRepos) Settings() store.SettingsRepo {
	return &settingsRepo{st: r.st, scope: r.scope, ds: r.current()}
}
