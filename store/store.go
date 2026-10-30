// Package store defines the persistence port.
//
// The kernel and the domain see only these interfaces, so replacing a backend
// changes exactly one adapter. Two implementations ship from day one,
// store/memory and store/anansistore, and both must pass the same conformance
// suite (storetest).
//
// The interfaces in this file are copied verbatim from the Store
// specification, section 6. The entity types they refer to live in the
// domain package and carry no persistence tags.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/asaidimu/workspace/domain"
)

// Scope selects which owner a repository is restricted to. The zero Scope is
// invalid; System is for maintenance and migration code only.
type Scope struct {
	Owner  string
	System bool
}

// Store is the root of the port.
type Store interface {
	Scoped(scope Scope) (Repos, error)
	Atomic(ctx context.Context, scope Scope, opts TxOptions, fn func(ctx context.Context, tx Tx) error) error
	Maintenance(scope Scope) (Maintenance, error)
	Migrator() Migrator
	Capabilities(ctx context.Context) (Capabilities, error)
	Close(ctx context.Context) error
}

// TxOptions bounds one transaction.
type TxOptions struct {
	ReadOnly  bool
	Retryable bool          // opt-in; fn must then be idempotent
	Timeout   time.Duration // zero selects the store default
}

// Tx is a transaction: a Repos plus commit and rollback hooks.
type Tx interface {
	Repos
	AfterCommit(hook func())
	AfterRollback(hook func())
}

// Repos is the set of repositories inside one scope or transaction.
type Repos interface {
	Conversations() ConversationRepo
	Turns() TurnRepo
	Contents() ContentRepo
	Roles() RoleRepo
	Blobs() BlobRepo
	Runs() RunRepo
	Receipts() ReceiptRepo
	Settings() SettingsRepo
}

// Order selects the direction of a paged listing.
type Order int

const (
	NewestFirst Order = iota
	OldestFirst
)

// PageRequest is a keyset page request. Cursor is opaque and deterministic
// per implementation.
type PageRequest struct {
	Limit  int
	Cursor string
	Order  Order
}

// Page is one page of results. NextCursor is empty when exhausted.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// TopicFilter is an exact-match, case-sensitive topic constraint. Any is a
// union, All an intersection, and both together are ANDed.
type TopicFilter struct {
	Any []string
	All []string
}

// ConversationRepo reads and writes conversations.
type ConversationRepo interface {
	Create(ctx context.Context, c domain.Conversation) (domain.Conversation, error)
	Get(ctx context.Context, id string) (domain.Conversation, error)
	List(ctx context.Context, f ConversationFilter, p PageRequest) (Page[domain.Conversation], error)
	Update(ctx context.Context, id string, expectedRev int64, patch ConversationPatch) (domain.Conversation, error)
	SetHead(ctx context.Context, id string, expectedRev int64, head *domain.TurnRef) (domain.Conversation, error)
	Delete(ctx context.Context, id string, expectedRev int64) error
}

// TurnRepo reads and writes the immutable turn tree.
type TurnRepo interface {
	Append(ctx context.Context, t domain.Turn) (domain.Turn, error)
	Get(ctx context.Context, ref domain.TurnRef) (domain.Turn, error)
	Versions(ctx context.Context, id string) ([]domain.TurnRef, error)
	Children(ctx context.Context, parent domain.TurnRef) ([]domain.TurnRef, error)
	Chain(ctx context.Context, head domain.TurnRef, opts ChainOptions) ([]domain.Turn, error)
}

// ContentRepo reads and versions content keys.
type ContentRepo interface {
	Put(ctx context.Context, c domain.Content, baseVersion int) (domain.Content, error)
	Get(ctx context.Context, ref domain.ContentRef) (domain.Content, error)
	List(ctx context.Context, f ContentFilter, p PageRequest) (Page[domain.Content], error)
	Versions(ctx context.Context, key string) ([]domain.ContentRef, error)
	Tombstone(ctx context.Context, key string, baseVersion int) error
}

// RoleRepo reads and writes roles.
type RoleRepo interface {
	Put(ctx context.Context, r domain.Role, expectedRev int64) (domain.Role, error)
	Get(ctx context.Context, id string) (domain.Role, error)
	List(ctx context.Context, f RoleFilter, p PageRequest) (Page[domain.Role], error)
	Delete(ctx context.Context, id string, expectedRev int64) error
}

// BlobRepo holds blob metadata and holder links. Bytes live in the blob
// store; this repository never sees them.
type BlobRepo interface {
	Register(ctx context.Context, b domain.Blob) (blob domain.Blob, created bool, err error)
	Get(ctx context.Context, id string) (domain.Blob, error)
	GetByChecksum(ctx context.Context, checksum string) (domain.Blob, error)
	SetRemote(ctx context.Context, id string, provider string, remote domain.RemoteRef) error
	AddRef(ctx context.Context, id string, holder domain.EntityRef) error
	RemoveRef(ctx context.Context, id string, holder domain.EntityRef) error
	Refs(ctx context.Context, id string) ([]domain.EntityRef, error)
	Orphans(ctx context.Context, olderThan time.Time, limit int) ([]domain.Blob, error)
	Delete(ctx context.Context, id string) error
}

// RunRepo is the durable record of agent executions, including the claim
// protocol that gives crash recovery.
type RunRepo interface {
	Create(ctx context.Context, r domain.Run) (domain.Run, error)
	Get(ctx context.Context, id string) (domain.Run, error)
	Checkpoint(ctx context.Context, id string, expectedStep int, state json.RawMessage) (domain.Run, error)
	// SetStatus moves a run between its non-terminal states (queued, running,
	// waiting) with a compare-and-swap on the step, so two workers cannot
	// both decide the run's state. Terminal statuses go through Finish.
	SetStatus(ctx context.Context, id string, expectedStep int, status domain.RunStatus) (domain.Run, error)
	Heartbeat(ctx context.Context, id string, worker string) error
	Finish(ctx context.Context, id string, status domain.RunStatus, failure string) (domain.Run, error)
	Claim(ctx context.Context, staleBefore time.Time, limit int, worker string) ([]domain.Run, error)
	ListActive(ctx context.Context, conversationID string) ([]domain.Run, error)
}

// ReceiptRepo is the idempotency store.
type ReceiptRepo interface {
	Reserve(ctx context.Context, key string, ttl time.Duration) (receipt domain.Receipt, created bool, err error)
	Complete(ctx context.Context, key string, result json.RawMessage) error
}

// SettingsRepo holds the workspace-wide defaults.
type SettingsRepo interface {
	Get(ctx context.Context) (domain.Settings, error)
	Put(ctx context.Context, s domain.Settings, expectedRev int64) (domain.Settings, error)
}

// SweepOptions bounds a garbage-collection pass.
type SweepOptions struct {
	Grace  time.Duration
	Limit  int
	DryRun bool
}

// SweepReport is what one sweep pass removed.
type SweepReport struct {
	Examined int
	Removed  int
}

// Maintenance is the garbage-collection surface.
type Maintenance interface {
	SweepTurns(ctx context.Context, opts SweepOptions) (SweepReport, error)
	SweepBlobs(ctx context.Context, opts SweepOptions) (SweepReport, error)
	SweepReceipts(ctx context.Context, now time.Time) (SweepReport, error)
}

// Migrator applies the store's own forward-only migration set.
type Migrator interface {
	Migrate(ctx context.Context) (MigrationReport, error)
	SchemaVersion(ctx context.Context) (int, error)
}

// TxLevel is the strongest isolation a backend offers.
type TxLevel int

const (
	TxNone TxLevel = iota
	TxSnapshot
	TxSerializable
)

// Capabilities is what the adapter probed from the backend. Behaviour never
// silently degrades: a missing capability takes a portable path with the same
// results.
type Capabilities struct {
	Transactions      TxLevel
	RecursiveQueries  bool
	AtomicIncrement   bool
	ConcurrentWriters bool
	MaxValueBytes     int64
}

// The sentinel errors every implementation must use. The kernel maps them to
// its own error codes: not-found to NotFound, conflict and already-exists to
// Conflict, invalid and too-large to Invalid, busy to Busy, everything else
// to Internal.
var (
	ErrNotFound      = errors.New("store: not found")
	ErrAlreadyExists = errors.New("store: already exists")
	ErrConflict      = errors.New("store: conflict")
	ErrInvalid       = errors.New("store: invalid argument")
	ErrBusy          = errors.New("store: busy, retry")
	ErrTxClosed      = errors.New("store: transaction closed")
	ErrUnsupported   = errors.New("store: unsupported by backend")
	ErrCorrupt       = errors.New("store: corrupt data")
	ErrTooLarge      = errors.New("store: value too large")
	ErrInternal      = errors.New("store: internal error")
)
