# Workspace Store Specification

Draft 0.1 · 15 October 2026 · Go AI workspace (standalone library) · Status: for review

## 1. Purpose and scope

The store is the persistence port of the standalone Go workspace library. It defines what the workspace needs from storage, not how storage works. It persists conversations, turns, content, blob metadata, roles, agent runs, idempotency receipts and settings.

In scope: the Go interfaces, their semantics, the conformance suite, the go-anansi adapter mapping, and an in-memory reference implementation.

Non-goals: blob bytes (a separate `BlobStore` port), retrieval and full-text ranking (a separate `Retriever` port), event delivery (the kernel bus), authentication and authorization, hestia integration, event sourcing, cross-owner sharing, and encryption at rest (a backend concern; a `Codec` hook is reserved).

Basis: TS `@asaidimu/utils-workspace` 9.0.0 (types and bundled implementation), go-anansi v8 (commit 9d0157d, 2026-10-03), hestia (commit b5f159a, 2026-09-19) and `asaidimu/blobs` v1.3.4. Nothing here has been compiled or run; unverified assumptions are listed in section 15.

## 2. Design principles

1. **Ports first.** The kernel and domain depend only on the `store` package interfaces. Domain types are plain Go structs. They may carry anansi schema tags, because DTOs belong to anansi's data toolkit, but they import no persistence code.
2. **Two implementations from day one.** `store/memory` is the executable reference. `store/anansistore` is the production adapter. Both must pass the same `storetest` conformance suite. A third backend is accepted only if it passes it too.
3. **Portability by capability, not assumption.** go-anansi abstracts the backend behind `base.Persistence` and its query DSL, and ships two backends today, SQLite and an in-memory ephemeral one. The adapter therefore uses only DSL operations plus one isolated raw query that is guarded by a capability probe and has a portable fallback. No SQLite-specific type, pragma or error code may appear outside that one file.
4. **Owner scoping is structural.** Every repository is bound to a `Scope`. There is no API that can read across owners except the explicit system scope used by maintenance.
5. **Immutable history, mutable pointers.** Turns, content versions and blob metadata are append-only. Conversation heads, roles and settings change only through compare-and-swap on a revision counter.
6. **No slow I/O inside a transaction.** Network calls, LLM calls and blob byte transfers never happen inside `Atomic`. SQLite-class backends have a single writer, and a long transaction would stall every conversation.
7. **Opaque payloads.** The store never interprets content values or turn blocks. It round-trips them losslessly.

## 3. Architecture

```
domain/                 pure types and rules, no store imports
kernel/                 commands, unit of work, events (depends on store interfaces)
store/                  interfaces, errors, capabilities, types (this spec)
store/storetest/        conformance suite, run against every implementation
store/memory/           reference implementation (maps + journal)
store/anansistore/      adapter over anansi base.Persistence
store/instrument/       tracing and metrics decorator
```

Dependency rule: `store` imports only `domain` and the standard library. `anansistore` is the only package that imports go-anansi's persistence packages. Generated anansi models stay internal to `anansistore` and never appear in an exported signature.

## 4. Conventions

- **IDs.** UUIDv7 strings from an injected `IDGenerator`, so tests are deterministic.
- **Time.** UTC from an injected `Clock`. Timestamps must round-trip at microsecond precision.
- **Owner.** An opaque string supplied by the caller (a user ID when hosted, a constant when single-user).
- **Payloads.** `json.RawMessage`, stored and returned byte-faithful in meaning: no numeric coercion (integers above 2^53 and decimals such as 0.1 survive), no field loss, unknown fields preserved. Key order is not guaranteed.
- **Pagination.** Keyset cursors, opaque to callers, stable under concurrent inserts. Default order is by (created time, ID).
- **Money.** Never stored as floating point. Cost is `int64` micro-units plus a currency code defaulting to "KES".
- **Revisions.** Mutable entities carry `Rev int64`, incremented by exactly 1 on each successful mutation.
- **Errors.** Sentinels, always wrapped with context so `errors.Is` works: `ErrNotFound`, `ErrAlreadyExists`, `ErrConflict` (revision mismatch or referential block), `ErrInvalid`, `ErrBusy` (transient, safe to retry), `ErrTxClosed`, `ErrUnsupported`, `ErrCorrupt`, `ErrTooLarge`, `ErrInternal`.

## 5. Entities

- **Conversation:** ID, Owner, Title, RoleID, Head (\*TurnRef, nil when empty), ForkedFrom (\*TurnRef), Topics, Attached (\[\]ContentRef), Status (active or archived), Rev, CreatedAt, UpdatedAt.
- **Turn (immutable):** Ref (ID, Version), Owner, Parent (\*TurnRef), Actor (kind: user, agent, tool or system, plus ID), Blocks (\[\]Block of Type, Key, Value), Usage (Model, InputTokens, OutputTokens, CostMicros, Currency), FinishReason, CreatedAt. Turns belong to the owner, not to a conversation.
- **Content:** Ref (Key, Version), Owner, Type, Mode (read, write, readwrite or internal), Topics, Value, Metadata (string map), Tombstoned, CreatedAt. Versions are immutable.
- **Role:** ID, Owner, Name, Persona, PreferenceKeys (\[\]ContentRef), Topics, Constraints (opaque JSON), Rev.
- **Blob (metadata only):** ID, Owner, Checksum ("sha256:\<hex>"), Size, MediaType, Name, Remotes (provider to RemoteRef of ID and ExpiresAt), CreatedAt.
- **EntityRef:** Kind (turn, content, conversation or role), ID, Version. Used as a blob holder.
- **Run (agent execution state):** ID, ConversationID, Status (queued, running, succeeded, failed or cancelled), Step, State (opaque JSON), Worker, HeartbeatAt, StartedAt, FinishedAt, Failure.
- **Receipt (idempotency):** Key, State (reserved or completed), Result, ExpiresAt.
- **Settings:** Owner, Language, DefaultRoleID, GlobalPreferenceKeys, Rev.

Logical constraints and indexes every implementation must honour:

- Turn: unique (owner, id, version); index (owner, parent id, parent version) for children lookup.
- Content: unique (owner, key, version); index (owner, type, mode).
- Conversation: index (owner, updated time, id).
- Blob: unique (owner, checksum); holder links unique on (blob, holder).
- Receipt: unique (owner, key). Run: index (status, heartbeat).
- Topic links: unique (owner, kind, entity, topic) with a lookup index on (owner, kind, topic).

## 6. Interfaces

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Scope struct {
	Owner  string
	System bool // maintenance and migration only; the zero Scope is invalid
}

type Store interface {
	Scoped(scope Scope) (Repos, error)
	Atomic(ctx context.Context, scope Scope, opts TxOptions, fn func(ctx context.Context, tx Tx) error) error
	Maintenance(scope Scope) (Maintenance, error)
	Migrator() Migrator
	Capabilities(ctx context.Context) (Capabilities, error)
	Close(ctx context.Context) error
}

type TxOptions struct {
	ReadOnly  bool
	Retryable bool          // opt-in; fn must then be idempotent
	Timeout   time.Duration // zero selects the store default
}

type Tx interface {
	Repos
	AfterCommit(hook func())
	AfterRollback(hook func())
}

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

type Order int

const (
	NewestFirst Order = iota
	OldestFirst
)

type PageRequest struct {
	Limit  int
	Cursor string
	Order  Order
}

type Page[T any] struct {
	Items      []T
	NextCursor string // empty when exhausted
}

type TopicFilter struct {
	Any []string // at least one match; empty means no constraint
	All []string // every topic must match; empty means no constraint
}

type TurnRef struct {
	ID      string
	Version int
}

type ContentRef struct {
	Key     string
	Version int // 0 means latest non-tombstoned on reads
}

type ConversationFilter struct {
	IDs          []string
	Topics       TopicFilter
	RoleID       string
	Status       string
	UpdatedAfter *time.Time
}

type ContentFilter struct {
	Keys              []string
	Types             []string
	Modes             []string
	Topics            TopicFilter
	AllVersions       bool
	IncludeTombstoned bool
}

type RoleFilter struct {
	IDs    []string
	Topics TopicFilter
}

type ChainOptions struct {
	Limit int      // keep only the newest Limit turns; 0 means all
	Until *TurnRef // stop before this ancestor (exclusive)
}

type ConversationRepo interface {
	Create(ctx context.Context, c Conversation) (Conversation, error)
	Get(ctx context.Context, id string) (Conversation, error)
	List(ctx context.Context, f ConversationFilter, p PageRequest) (Page[Conversation], error)
	Update(ctx context.Context, id string, expectedRev int64, patch ConversationPatch) (Conversation, error)
	SetHead(ctx context.Context, id string, expectedRev int64, head *TurnRef) (Conversation, error)
	Delete(ctx context.Context, id string, expectedRev int64) error
}

type TurnRepo interface {
	Append(ctx context.Context, t Turn) (Turn, error)
	Get(ctx context.Context, ref TurnRef) (Turn, error)
	Versions(ctx context.Context, id string) ([]TurnRef, error)
	Children(ctx context.Context, parent TurnRef) ([]TurnRef, error)
	Chain(ctx context.Context, head TurnRef, opts ChainOptions) ([]Turn, error)
}

type ContentRepo interface {
	Put(ctx context.Context, c Content, baseVersion int) (Content, error)
	Get(ctx context.Context, ref ContentRef) (Content, error)
	List(ctx context.Context, f ContentFilter, p PageRequest) (Page[Content], error)
	Versions(ctx context.Context, key string) ([]ContentRef, error)
	Tombstone(ctx context.Context, key string, baseVersion int) error
}

type RoleRepo interface {
	Put(ctx context.Context, r Role, expectedRev int64) (Role, error)
	Get(ctx context.Context, id string) (Role, error)
	List(ctx context.Context, f RoleFilter, p PageRequest) (Page[Role], error)
	Delete(ctx context.Context, id string, expectedRev int64) error
}

type BlobRepo interface {
	Register(ctx context.Context, b Blob) (blob Blob, created bool, err error)
	Get(ctx context.Context, id string) (Blob, error)
	GetByChecksum(ctx context.Context, checksum string) (Blob, error)
	SetRemote(ctx context.Context, id string, provider string, remote RemoteRef) error
	AddRef(ctx context.Context, id string, holder EntityRef) error
	RemoveRef(ctx context.Context, id string, holder EntityRef) error
	Refs(ctx context.Context, id string) ([]EntityRef, error)
	Orphans(ctx context.Context, olderThan time.Time, limit int) ([]Blob, error)
	Delete(ctx context.Context, id string) error
}

type RunRepo interface {
	Create(ctx context.Context, r Run) (Run, error)
	Get(ctx context.Context, id string) (Run, error)
	Checkpoint(ctx context.Context, id string, expectedStep int, state json.RawMessage) (Run, error)
	Heartbeat(ctx context.Context, id string, worker string) error
	Finish(ctx context.Context, id string, status RunStatus, failure string) (Run, error)
	Claim(ctx context.Context, staleBefore time.Time, limit int, worker string) ([]Run, error)
	ListActive(ctx context.Context, conversationID string) ([]Run, error)
}

type ReceiptRepo interface {
	Reserve(ctx context.Context, key string, ttl time.Duration) (receipt Receipt, created bool, err error)
	Complete(ctx context.Context, key string, result json.RawMessage) error
}

type SettingsRepo interface {
	Get(ctx context.Context) (Settings, error)
	Put(ctx context.Context, s Settings, expectedRev int64) (Settings, error)
}

type SweepOptions struct {
	Grace time.Duration
	Limit int
	DryRun bool
}

type SweepReport struct {
	Examined int
	Removed  int
}

type Maintenance interface {
	SweepTurns(ctx context.Context, opts SweepOptions) (SweepReport, error)
	SweepBlobs(ctx context.Context, opts SweepOptions) (SweepReport, error)
	SweepReceipts(ctx context.Context, now time.Time) (SweepReport, error)
}

type Migrator interface {
	Migrate(ctx context.Context) (MigrationReport, error)
	SchemaVersion(ctx context.Context) (int, error)
}

type TxLevel int

const (
	TxNone TxLevel = iota
	TxSnapshot
	TxSerializable
)

type Capabilities struct {
	Transactions      TxLevel
	RecursiveQueries  bool
	AtomicIncrement   bool
	ConcurrentWriters bool
	MaxValueBytes     int64
}

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
```

`ConversationPatch`, `Turn`, `Content`, `Role`, `Blob`, `Run`, `Receipt`, `Settings`, `RunStatus`, `RemoteRef`, `MigrationReport` and `EntityRef` are the types described in section 5, defined in `domain` or `store` and carrying no persistence tags.

## 7. Semantics

### 7.1 Transactions

- `Atomic` is all-or-nothing across every repository in the `Tx`. A returned error or a panic rolls back; the panic is re-raised after rollback.
- Reads inside the `Tx` see its own writes. Isolation is at least snapshot. A backend reporting `TxNone` is rejected at construction unless explicitly allowed for tests.
- A nested `Atomic` called with the `ctx` passed to `fn` joins the outer transaction. Hooks attach to the outermost transaction.
- `AfterCommit` hooks run once, in registration order, after the commit is durable, on the committing goroutine. A panicking hook is contained and logged and does not affect the others. `AfterRollback` hooks run after rollback, including on the panic path. The kernel uses `AfterCommit` to publish domain events and `AfterRollback` for compensating external effects.
- There is no automatic retry. `Retryable` re-runs `fn` only on `ErrBusy`, and the caller promises `fn` is idempotent.
- `fn` must not perform network, LLM or blob byte I/O. A default timeout (5 s) bounds transactions; exceeding it returns `ErrBusy` and rolls back.

### 7.2 Concurrency control

Mutations of conversations, roles and settings are compare-and-swap on `Rev`. A mismatch returns `ErrConflict` and changes nothing. The kernel's per-conversation serializer is a throughput optimization only. Correctness must hold with several processes sharing one database.

### 7.3 Turn history

- `Append` inserts an immutable turn. It fails with `ErrAlreadyExists` on a duplicate (ID, Version) and with `ErrNotFound` if `Parent` is set and absent.
- An edit is a new Version of an existing ID. All versions of an ID share the logical position of version 1; the exact parent invariant is validated against the TS fixtures in Phase 0.
- `Chain` walks parent pointers from `head` and returns turns oldest to newest. With `Limit` it returns the newest `Limit`; with `Until` it stops before that ancestor. A missing ancestor or a cycle returns `ErrCorrupt`. It issues one query when `RecursiveQueries` is true, otherwise it fetches ancestors in rounds.
- `Children` returns every version of every child of a parent ref. `Versions` returns all refs of an ID in ascending order. Together they support branch navigation.
- Forking is creating a conversation whose `Head` points at an existing turn. No turn is copied.

### 7.4 Content versioning

`Put` creates version `baseVersion + 1` of a key. `baseVersion` must equal the current latest (0 for a new key), otherwise `ErrConflict`. `Tombstone` appends a tombstoned version and hides the key from default reads and lists.

### 7.5 Deletion and garbage collection

- Deleting a conversation removes its row, topic links and attached-content links. It never deletes turns, because forks may share them.
- `SweepTurns` removes turns older than `Grace` that are unreachable from every conversation head, every `ForkedFrom`, and every active run's pinned refs. Reachability is computed by walking from all heads.
- `SweepBlobs` removes blob metadata with zero holders older than `Grace`. `BlobRepo.Delete` returns `ErrConflict` if holders exist.
- Sweeps are idempotent, resumable and safe to run concurrently with normal traffic. `DryRun` reports without deleting.

### 7.6 Blobs

The store holds metadata only; bytes live in `BlobStore`. The required order is: write bytes, then register metadata and add holder links inside one `Atomic`. A crash between the two leaves orphan bytes, which the `BlobStore` reconcile pass removes. `Register` deduplicates by (owner, checksum) and returns the existing record with `created=false`. `AddRef` and `RemoveRef` are idempotent.

### 7.7 Idempotency

`Reserve` returns `created=true` exactly once per (owner, key) while unexpired. Later callers receive the existing receipt, either reserved (an execution is in flight) or completed with its stored result. `Complete` stores the result. Expired receipts are removed by `SweepReceipts`.

### 7.8 Runs

`Checkpoint` is compare-and-swap on `Step` and increments it. `Claim` atomically reassigns running runs whose heartbeat is older than `staleBefore` to the new worker, so each run has at most one live claimant. This gives crash recovery without relying on any transport's best-effort execution.

### 7.9 Topics

Topics are an exact-match, case-sensitive set; callers normalize Unicode before passing them in. `Any` is a union, `All` an intersection, and both together are ANDed. Semantics must be identical in every implementation. The old TS bug, where an entity in several topics was diffed against only its first topic, is covered by a dedicated test.

## 8. Capabilities and portability

The adapter probes the backend once at startup and publishes `Capabilities`. Behaviour never silently degrades: when a capability is missing, a portable path with the same results is used and logged.

- **RecursiveQueries false:** `Chain` fetches ancestors in rounds, with the same results and more round trips.
- **AtomicIncrement false:** revision increments use read-modify-write inside the transaction.
- **ConcurrentWriters false:** the adapter serializes writers and enforces a stricter default transaction timeout.
- **TxNone:** construction fails.

Because the interface is independent of anansi, a future non-anansi backend (a native Postgres, Pebble or other implementation) needs only to satisfy the interfaces and pass `storetest`.

## 9. anansi adapter

- **Construction.** `anansistore.New(p base.Persistence, opts)`. The caller owns the persistence lifecycle: standalone code opens it from anansi's backend factory, and a host such as hestia can pass its own instance. Options: collection prefix (default `ws_`), `Clock`, `IDGenerator`, logger.
- **Collections.** `ws_conversations`, `ws_turns`, `ws_contents`, `ws_roles`, `ws_blobs`, `ws_blob_refs`, `ws_runs`, `ws_receipts`, `ws_settings`, `ws_topic_links`. Schemas are anansi schema JSON files with generated models kept internal. Indexes are the section 5 set, using composite, unique and partial indexes.
- **Payload storage.** Kind-plus-payload data follows anansi's own modeling rule: a type string (an open set, so not an enum) plus a payload field, and never a union, which anansi's domain-modeling guide warns against. Free-form JSON that must keep exact numbers, such as tool arguments and outputs, run state and role constraints, is stored as an opaque string until Phase 0 shows that record fields round-trip numbers exactly.
- **Transactions.** `Persistence.Transact`; nested calls join through the context. `Tx` hooks are wired to the transaction's commit and rollback callbacks.
- **Compare-and-swap.** anansi bumps the document's metadata version atomically on every update and offers a version option as an optimistic lock, so `Rev` maps to that version and no custom field is needed. A supplied update filter replaces the id filter instead of adding to it, so the adapter includes the id condition itself. A version mismatch must map to `ErrConflict`, and Phase 0 confirms how it is reported.
- **Raw query isolation.** Exactly one file, `chain.go`, may use the raw-query path for a recursive ancestor query. It uses collection placeholders and bound parameters only, is enabled by the capability probe, and falls back to the iterative path if the template is rejected.
- **Pooled documents.** Every anansi document is converted to a domain struct and released before the method returns. A test fails the build if a pooled document escapes.
- **Errors.** One translation table maps anansi errors to the section 4 sentinels, with the cause preserved. Transient and lock errors map to `ErrBusy`. Unknown errors become `ErrInternal`.
- **Migrations.** `Migrate` is idempotent and forward-only. It applies the workspace's own migration set, records `SchemaVersion`, and refuses to run against a newer schema than it knows. Hosted deployments invoke it from the host's migration hook.
- **Backends.** SQLite and an in-memory ephemeral backend ship with go-anansi, and the contract suite runs on both. Another backend requires an implementation of anansi's database interactor, and `storetest` is its acceptance gate.

The adapter also follows anansi's data-layer rules, taken from its schema and domain-modeling documentation:

- **Schemas and models.** Each collection is an anansi schema JSON file, which is the source of truth. `anansi codegen golang` generates the models, and a `projections` block in the schema generates narrow shapes. Models and shapes stay internal to `anansistore`.
- **Shapes for hot reads.** A turn header shape (every field except the blocks) serves children, versions, branch navigation and chain walks that need no content. A conversation summary shape serves lists. `ReadAs`, `CreateFrom` and `UpdateFrom` bind these shapes and release pooled documents for you. Scans over thousands of rows use raw documents with `Get` and `Release` and no binding, as the anansi guide advises.
- **Mapping.** Domain types are mapped to models and shapes by generated or hand-written functions, never by runtime reflection in the domain, and a round-trip property test covers every entity.
- **Model cache.** `ModelCollection` offers a read-through cache with negative caching and TTLs, keyed by document ID. The adapter uses it for immutable turns and content versions only if the document ID can be the (id, version) key. Otherwise the domain keeps its own immutable cache.
- **Capabilities.** Backends report anansi's `query.Capabilities`: concurrency model, retry policy, pagination types and nested-field support. The adapter derives `Store.Capabilities` from it. The ephemeral backend supports only offset pagination, so keyset cursors are built from comparison filters on (created time, ID).
- **Money.** Money is never a `decimal` field. It is int64 micro-units, as the anansi guide recommends, and the SQLite backend maps `decimal` columns to `REAL`.

## 10. In-memory implementation

Maps guarded by one mutex, with a write journal for rollback. It reports `TxSerializable`, uses the same sentinels and injected clock and ID generator, and encodes cursors deterministically. It must pass the full suite under `-race`. It is the default store for unit tests and the oracle for differential testing against `anansistore`.

anansi's ephemeral backend is a second in-memory option, and it exercises the real adapter. The contract suite therefore runs on the memory store, on anansi with the ephemeral backend, and on anansi with SQLite.

## 11. Conformance suite

`storetest.Run(t, factory)` covers:

1. Atomicity, rollback on error and on panic, read-your-writes, nested join, hook ordering and containment.
2. Compare-and-swap under N concurrent writers: exactly one wins, the rest get `ErrConflict`.
3. Turn immutability, duplicate detection, missing parent, cycle and corruption detection.
4. `Chain` against a reference model, with `Limit` and `Until`, depth 1, 2, 500 and 5,000, with and without recursive support.
5. Fork shares turns, deleting a fork or its source never affects the other, and reachability GC keeps shared history.
6. Content versioning, base-version conflicts and tombstones.
7. Topic `Any`, `All` and combined filters, including multi-topic entities.
8. Keyset pagination stability while rows are inserted, in both orders.
9. Lossless payload round trip: integers above 2^53, decimals, nested and unknown fields, large values, Unicode and null bytes.
10. Owner isolation: no repository method returns or modifies another owner's data; system scope is the only exception.
11. Blob dedupe, holder idempotency, orphan detection and delete-with-holders.
12. Receipt reservation races and expiry.
13. Run claim exclusivity under concurrent claimants, checkpoint compare-and-swap, and recovery after a simulated crash.
14. Migration idempotence and refusal of newer schemas.
15. Differential test: random operation sequences applied to the memory store and the adapter must give identical observable results.
16. Fuzzing of cursors and payload decoding; `ErrTooLarge` above `MaxValueBytes`.

## 12. Non-functional targets

These are targets to validate in the spike, not measurements.

- `Append` costs at most 3 statements or round trips.
- `Chain` of 500 turns is one query when recursion is supported; otherwise a number of rounds bounded by a configurable batch size.
- `Chain` with `Limit n` never loads more than n+1 rows.
- List operations cost O(page size), never O(table size).
- Indicative p95 on a local SSD with SQLite: `Append` under 5 ms, `Chain(500)` under 30 ms.
- Memory use per call is bounded by the result size.

## 13. Security and privacy

- The adapter injects the owner into every query and write, and cross-owner tests run on every implementation.
- System scope is constructed only in maintenance and migration code paths and is audited.
- No SQL is assembled from caller input. Raw templates use placeholders and bound parameters only.
- Payloads are never logged. Logs carry IDs, sizes and durations.
- A configurable cap (default 8 MB per value) returns `ErrTooLarge`.
- Provider credentials are never stored here. Encryption at rest is delegated to the backend, with a reserved `Codec` hook for field-level encryption of payloads.

## 14. Observability

`store/instrument` decorates `Store` and every repository. It emits a span and a duration histogram per operation, error-class counters, transaction duration, retry and conflict counts, and a warning for transactions over half the timeout. Attributes never include payloads.

## 15. Items to verify before implementation

1. How `OnCommit` and `OnRollback` are reached from inside the `Persistence.Transact` callback.
2. How a version mismatch on update is reported, and whether it maps cleanly to `ErrConflict`.
3. Raw template parameter binding, the recursive query's cost, and how the adapter detects the capability.
4. Whether record fields preserve exact numbers and unknown fields. Until that is shown, open JSON stays an opaque string.
5. Snapshot semantics and read-your-writes inside `Transact`, and writer contention across concurrent conversations on SQLite.
6. Performance of topic `All` queries through the junction collection, and whether an in-memory intersection fallback is needed.
7. Keyset cursor stability on (created time, ID) under concurrent inserts.
8. The edit-version parent invariant against TS fixtures.
9. The overhead of the pooled-document release discipline.
10. Whether the ephemeral backend passes the full contract suite, including transactions. It exists and implements the interactor, but supports only offset pagination.

Three more items came from reading anansi's data layer:

- Whether the document ID can be chosen by the caller, so that (id, version) can be the key and the model cache applies to turns.
- Whether a projection shape reads only the projected columns from SQLite, so a turn header read skips the blocks.
- How the model cache behaves around transactions, in particular whether a rolled-back write is evicted.

## 16. TS behaviours this spec corrects

- A forked session shared a head pointer but not turns, because turns were filtered by session. Here turns belong to the owner.
- Deleting a session removed only its record. Here deletion plus reachability GC handles turns, and holder links handle blobs.
- Releasing a blob never purged it at zero references. Here `Orphans`, `SweepBlobs` and `Delete` define the lifecycle.
- Listing a session's turns scanned the whole collection and filtered in memory. Here all reads are index-backed.
- Topic index diffs looked only at an entity's first topic. Here topic semantics are specified and tested.
- Token and cost accounting was stubbed. Here `Usage` is a first-class field with currency-tagged integer cost.

## 17. Acceptance criteria

- `store/memory` and `store/anansistore` both pass `storetest`, including the differential test, under `-race`.
- All section 15 items are resolved and recorded as findings or upstream tickets.
- The spike benchmark report meets or explains every section 12 target.
- No go-anansi persistence import exists outside `store/anansistore`, and no pooled document escapes it (enforced by tests).
- A written decision records how a third backend would be added and validated.
