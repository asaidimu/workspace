# Phase 2: store/anansistore (production adapter) + blob reconcile

## Context

Spec: `todo/specs/Store specification.md` §9 (anansi adapter), §10 (memory as the
differential oracle), §11 (conformance suite), §15 (unverified assumptions
checked by the spike programs in `spike/`). Delivery plan: `todo/specs/Workspace
specification.md` §17 Phase 2 — gate: "the same conformance suite passes on
`anansistore`, the differential test is green, and the blob reconcile job
works."

State so far: facade milestone delivered (`todo/done/facade.md` all `[*]`).
`store/memory` + `store/storetest` conformance green under `-race`. The port
lives in `store/store.go` / `store/types.go`; domain types are plain stdlib
structs. `github.com/asaidimu/go-anansi/v8 v8.10.2` is pinned in go.mod but
only `spike/*`, `content`, `kernel/errors` import parts of it today.

Dependency rule (§7 of Store spec): **`store/anansistore` is the only package
that imports go-anansi persistence packages; generated/derived models stay
internal — never in an exported signature.** `boundary/boundary_test.go`
enforces non-test imports, so keep anansi out of every other non-test file.

## Key spec facts (from §9)

- Construction: `anansistore.New(p base.Persistence, opts)`; caller owns
  persistence lifecycle (backend factory for standalone, hestia passes its
  own). Options: collection prefix (default `ws_`), Clock, IDGenerator,
  logger.
- Collections: `ws_conversations`, `ws_turns`, `ws_contents`, `ws_roles`,
  `ws_blobs`, `ws_blob_refs`, `ws_runs`, `ws_receipts`, `ws_settings`,
  `ws_topic_links`. Indexes = §5 set (composite/unique/partial).
- Payloads: `type string` (open set) + payload field, never a union.
  Free-form JSON needing exact numbers (tool args/outputs, run state, role
  constraints) stored as **opaque string** until proven to round-trip.
- Transactions: `Persistence.Transact`, nested calls join via context; Tx
  hooks wired to commit/rollback.
- CAS: anansi bumps doc metadata version on update + version option as
  optimistic lock → maps to port `Rev`. Supplied update filter REPLACES the
  id filter → adapter must include id condition itself. Version mismatch →
  `store.ErrConflict` (how it is reported = Phase 0 spike finding).
- Raw query isolation: exactly one file (`chain.go`) may use raw query for
  the recursive ancestor chain; capability-probed with iterative fallback.
- Pooled documents: convert to domain struct and `Release` before return; a
  test fails the build if a pooled doc escapes.
- Errors: ONE translation table → port sentinels (`ErrNotFound`, `ErrConflict`,
  `ErrInvalid`, `ErrForbidden`, `ErrBusy` for transient/lock, `ErrInternal`
  unknown), cause preserved.
- Migrations: `Migrate` idempotent, forward-only, records `SchemaVersion`,
  refuses newer schema than it knows.
- Backends: contract suite runs on memory, anansi **ephemeral** (in-memory),
  and anansi **SQLite**.
- Schemas: schema JSON is source of truth + `anansi codegen golang`; BUT
  `AGENTS.md` rule (learned hard against v8.6.4/v8.x): **prefer a tagged Go
  struct + `data.ExtractDTOSchemaDirect(&T{})` over hand-writing schema JSON**
  (field IDs must be UUIDv7 — derivation mints them). Decide which route the
  anansi skill recommends for v8.10.2 and follow it; models stay internal.
- Shapes/projections: turn header shape, conversation summary shape;
  `ReadAs`/`CreateFrom`/`UpdateFrom`; scans over thousands of rows use raw
  docs with Get/Release.
- Model cache: only for immutable turns/content if doc ID can be
  (id, version); else domain keeps its own cache.
- Capabilities: derive `Store.Capabilities` from backend `query.Capabilities`;
  ephemeral = offset pagination only → keyset cursors from comparison filters
  (created time, ID).
- Money: int64 micro-units, never decimal.

## Differential + reconcile (gate items)

- **Differential test**: memory store is the oracle; run the same storetest
  scenario corpus (or a dedicated shadow-write suite) against both memory and
  anansistore and compare outcomes. Spec §11/§17.
- **Blob reconcile job**: bytes-then-metadata order (§metadata ~line 334) can
  leave orphan bytes on crash; a reconcile pass removes bytes no metadata
  references. MemoryBlobs/`blob.Store` need a List + reconcile entry point;
  gate: "the blob reconcile job works" (test with an orphan injected).
- Contract suite must run on all three backends (memory, ephemeral, sqlite).

## Steps

- [*] Read `todo/specs/Store specification.md` §11 conformance list + §15 assumptions and
      the `spike/` findings already committed; list which §15 anansi
      assumptions are still open for the adapter (transact nesting, version
      mismatch reporting, number round-trip, update counts, recursive query).
- [*] Load the `anansi` skill and survey go-anansi v8.10.2 APIs actually in
      the module cache: `base.Persistence`, `Transact`, query DSL,
      `ModelCollection`, projections, capability probe, backend factory
      (ephemeral + SQLite), error taxonomy. Record the real signatures in this
      file's notes as they replace spec guesses.
- [*] Decide schema route (struct-derived via `ExtractDTOSchemaDirect` vs
      `.schema.json` + codegen) per skill guidance; create the 10 collections
      with prefix `ws_` through `anansistore.New` (idempotent ensure/migrate).
- [*] `store/anansistore` skeleton: `New(p, Options)`, port surface
      (`store.Store`: Atomic/Scoped/Repos/Maintenance/Migrator/Capabilities),
      error translation table, document↔domain mapping helpers (internal).
- [*] Implement repos in port order: conversations (fork/list/pagination,
      head CAS), turns (versions/children/chain/branch navigation, optimistic
      lock → ErrConflict), contents (immutable versions, tombstone), roles,
      receipts (idempotency), settings, runs (claim/heartbeat/checkpoint CAS),
      blobs + blob_refs (register dedupe by owner+checksum, AddRef/RemoveRef
      idempotent), topic_links.
- [=] `chain.go`: raw recursive ancestor query behind capability probe with
      iterative fallback — the only raw-query file allowed. **Skipped:**
      anansi's `query.Capabilities` exposes no recursion flag to probe, and
      Store spec §8 explicitly blesses `RecursiveQueries false` + round-based
      Chain as the portable path (same results, more round trips). The
      adapter reports false honestly; storetest + differential prove
      identical chain results. If a raw-recursion path is ever wanted, it is
      a self-contained addition behind this same capability.
- [*] Cursors/capabilities mapping; opaque-string payload fields for
      tool args/outputs, run state, role constraints.
- [*] `Migrate`/`SchemaVersion` forward-only.
- [*] Wire storetest: `store/anansistore` tests run the shared conformance
      suite on (a) ephemeral backend, (b) SQLite (temp file or :memory:),
      reusing `store/storetest.Run` with a factory.
- [*] Differential test: same corpus against `store/memory` and anansistore;
      compare results (sentinels, values, ordering) — green under `-race`.
- [*] Blob reconcile: implement the pass (remove bytes with no metadata
      holder), test by injecting an orphan into `workspacetest.MemoryBlobs`
      (and any other blob store exposing List) — gate item.
- [*] No-pooled-document test + boundary check: `grep`/test that anansi
      persistence imports exist only under `store/anansistore`; run
      `boundary_test.go`.
- [*] Full gate: `gofmt -l .`, `go build ./...`, `go vet ./...`,
      `go test -race -count=3 -shuffle=on ./...` — ALL GREEN (ephemeral
      backend excluded per notes: spike decision 1; sqlite + memory
      included).

## Files

- Create: `store/anansistore/` (adapters per repo or grouped, `chain.go`,
  `errors.go`, `schema`/models internal, `anansistore_test.go` with the three
  backend factories, differential test file), `todo/done/anansistore.md` (this).
- Modify: possibly `store/storetest` (factory signature already exists?),
  `blob/` (reconcile API if not present), `workspacetest` (orphan fixture).
- Do NOT touch: `go.mod` pins (no `go mod tidy`).

## Notes (verified against the spikes, skill references and module source)

- **Ephemeral backend is OUT.** `docs/spike.md` decision 1: it has a
  projection bug (`docs/upstream.md` §2) and quadratic writes; the contract
  suite runs on **SQLite in-memory** instead. Factory = hand-built
  `sql.Open("sqlite3", ":memory:")` + `sqlite/executor` + `sqlite/query` +
  `native.NewNativeInteractor` + `persistence.NewPersistence` (bypasses the
  `Playground`/`Setup` process singletons; spike/misc shows the pieces).
  `db.SetMaxOpenConns(1)` keeps one in-memory DB and serializes writers.
- **Caller-chosen `_id_` is rejected** unless 32-hex dashless UUID
  (spike/records S11) → composite `id@version` document keys are impossible.
  **Design: anansi mints `_id_`; domain identity lives in unique scalar
  fields** (`conversation_id`, `turn_key` = `id@version`, `content_key` =
  `key@version`, `role_id`, `blob_id`, `ref_key`, `run_id`, `receipt_key`,
  `settings_key`). Uniqueness via schema `unique: true`; duplicate →
  `ERR_UNIQUE_CONSTRAINT_VIOLATION` → `ErrAlreadyExists`.
- **CAS = `count==0 err=nil` → translate to `ErrConflict` myself**
  (spike/tx §15.2). Pre-read inside the same mini-transaction to distinguish
  `ErrNotFound` from stale-rev conflict.
- **Model cache disabled** (cache poisoning, spike/records S13) — raw
  `base.Collection` only, no `ModelCollection`.
- **Free-form JSON as opaque STRING fields** (big-int precision loss,
  §15.4): blocks, usage, parent/head refs, actor, state, results,
  constraints, remotes, topics, attached/preference refs.
- **Keyset paging on the domain sort key** (cid/role_id/content_key+version),
  cursor = last key, `OrderBy dir` + `cond(key, Gt/Lt, cursor)`, fetch
  limit+1 to decide `NextCursor` exactly like memory's `paginate`
  (NewestFirst = desc window, NextCursor = highest key). topics +
  heartbeat + anything non-scalar = residual filtering in Go (mirrors
  memory's `topicsMatch`).
- **`Atomic`**: `p.Transact(ctx[+Timeout deadline], fn)`; handler gets
  `tctx` (kernel passes fn's ctx through — `kernel/dispatch.go:43`); tx
  repos capture `tctx` and use it for every anansi call (context is the
  transaction carrier); `AfterCommit/AfterRollback` →
  `transaction.GetCurrentTransaction(tctx).OnCommit/OnRollback`
  (spike-verified: fires exactly once).
- **Scoped writes wrap read+write in their own mini `p.Transact`** so
  read-modify-write (Claim, CAS updates) is atomic; inside an open port tx
  the mini-transact joins flat (spike §15.1). Busy/locked → `ErrBusy`, with
  a bounded no-sleep retry inside Claim so storetest's 8-way race sees no
  error (single-conn sqlite serializes; the Go pool queues waiters).
- **`Store.Close` does not close `p`** — caller owns the lifecycle (spec §9);
  test factory closes the `sql.DB` in `t.Cleanup`.
- **Memory is the semantic oracle** — mirror quirks exactly: Create scope
  mismatch = `ErrInvalid` (not Forbidden); `Get` is NOT owner-filtered,
  `List`/`Orphans` ARE; runs/receipts unscoped; settings keyed by
  owner|system; content head = max version incl. tombstoned; tombstone
  marks ALL versions; role/settings Put create iff expectedRev==0 else CAS;
  receipt default TTL 24h; Maintenance sweeps are no-ops (parity);
  Chain `Until` is INCLUSIVE of the until turn (storetest wins over the
  types.go comment); `Versions`/`Children` sorted (id, version) in Go.
- **Capabilities**: TxSerializable, AtomicIncrement true, ConcurrentWriters
  true, RecursiveQueries **false** (chain is iterative; storetest only
  requires ≥ TxSnapshot), MaxValueBytes 0.
- **Schemas**: struct-derived via `data.SchemaFrom[T]` (UUIDv7 field ids,
  production-valid without ANANSI_ENV), then set `Name` to the spec
  collection name (`ws_*`) before `CreateCollection`. Writes use
  `data.NewDocument(map)` with the same field names (spike-proven);
  round-trip coverage comes from storetest + a full-field roundtrip test.
  `data.ConfigureDocumentFactory` runs once in `New` (singleton; required or
  persistence panics).
- **Dates**: RFC3339Nano strings, always written through the injected clock
  exactly where memory uses `st.now()` (Create/Update/Heartbeat/Finish...).
- Kernel passes the Atomic fn's ctx to handlers (`kernel/dispatch.go:43`), so
  tx binding via captured `tctx` is belt-and-braces, not a workaround.

- **`CollectionUpdate` mechanics (verified, probe matrix then removed):**
  `Count` is `len(ReturnDocument rows)` — always 0 unless
  `ReturnDocument: &true` is set; every Count check must set it (`rdPtr()`).
  `Version: nil` = no CAS (applies to all matches); `Version: &n` stale =
  count 0, err nil, not applied. Reads with 0 rows return err nil (adapter
  pre-reads to distinguish NotFound); `Total` is garbage, never used.
- **NULL vs empty (verified):** absent optional column → `HasKey=false`,
  `GetString` errors `ERR_DATA_KEY_NOT_FOUND`, `GetOr` returns the default;
  `Set(key, nil)` writes SQL NULL on a nullable column; `Set(key, nil)` on a
  NOT NULL column errors `ERR_PERSISTENCE_UPDATE_DOCUMENTS_FAILED`; `Set(key,
  "")` stores the empty string. Decode helpers treat absent as zero/nil —
  matches memory. Role full-replace clears optional fields by sending
  explicit nil (`roleReplaceDoc`), proven by the differential corpus.
- **Update-with-Set idiom:** partial Sets skip required-field validation, so
  a Set may carry a subset of keys; a full replace must include every key it
  wants to keep (omitted keys keep their old value) — that is why
  conversations mutate/SetHead pass the complete document or the explicit
  clear keys.
- **Oracle quirks pinned this session** (all mirrored): `Tombstone` on a
  missing key is `ErrNotFound` (head==0 check before CAS); role/settings Put
  on an existing row with the wrong rev is `ErrConflict` (AlreadyExists only
  for true create-dups like conversation/turn unique keys); blob `Register`
  validates checksum+scope first (`ErrInvalid`), dedupes by checksum
  GLOBALLY before the ID check, then `ErrAlreadyExists`; `Claim` validates
  `worker!=""` and `limit>=1`; memory `Chain` appends the Until turn then
  breaks (Until INCLUSIVE — types.go's "exclusive" comment is wrong; code
  wins); `paginate` sorts by key asc before the window, NewestFirst reverses
  only the display, NextCursor = highest key of the window.
- **Read-only tx semantics:** memory stages writes on a clone, DISCARDS it
  on success (no error for writes inside a RO tx!) and still runs COMMIT
  hooks; fn error → rollback hooks + err. The adapter mirrors this with
  `errReadOnlyRollback`: anansi rolls the tx back, then the port runs commit
  hooks and returns nil. Differential asserts byte-identical behavior.
- **Differential coverage:** storetest has no Role/Blob/Receipt/Settings
  sections (9 sections total: Conversation, Turn, Content, Topics, Paging,
  Tx, Run, Capabilities, ConcurrentAtomic) — memory has no dedicated tests
  for those repos either (kernel tests cover them). `differential_test.go`
  drives ~110 named steps over every repo + edge cases (nil-clears, dedupe,
  expiry, claims, hooks) with sentinel-classified errors, cursor-emptiness
  page chaining, and normalized JSON values (`canon` folds nil/empty
  containers; `Page.NextCursor` compared only for exhaustion).
- **Blob reconcile:** `blob/reconcile.go` — `Reconciler` + `ChecksumLookup`
  (system-scope `GetByChecksum` adapter wired in `Open`, closure over
  `store.Scoped({System:true})`). `blob.Info` carries no timestamp, so
  grace = first-sighting state held on the Reconciler (restart restarts the
  window — safe: worst case one more cycle); `Grace: 0` deletes on first
  sight. Wired into `admin.Sweep` with `SweepGrace`; `SweepSummary.Bytes`
  counts removed byte files. Tests: `blob/reconcile_test.go` (unit, fake
  clock) + `TestAdminSweepReconcilesOrphanBytes` (end to end through the
  harness, 25h advance past the 24h default grace). The hourly background
  goroutine (spec §12) is NOT part of this phase — no scheduler exists yet;
  that belongs to the facade/scheduling work.
- **Escape test:** `escape_test.go` parses the package with go/ast and fails
  if an exported signature or any struct field mentions `data.Documenter` /
  `document.Document`; mutation-verified (temporary violating func trips it).
  Runtime use-after-release is covered by contract + differential (a released
  document reads as cleared data).
- **Probe file removed** (`probe_test.go`): its findings are recorded above;
  TestContract + TestDifferential now guard every assumption it pinned.
- **Session results:** TestContract 9/9 sections green on SQLite in-memory;
  TestDifferential green (one adapter fix found: role full-replace nil
  clears); blob reconcile unit + e2e green; boundary green; escape green.
