# Spike findings (Store specification §15)

Every spike is a program under `spike/`, run against sqlite in-memory via
`anansi.Playground` (`DBPath: ":memory:"`). Run one with:

```
ANANSI_ENV=development go run ./spike/<name>
```

The ephemeral backend is not used: it has a projection bug (see
`docs/upstream.md` §2) and its writes are quadratic (it deep-copies the whole
store per transaction, so a bulk seed crawls). SQLite in-memory is the store
for both spikes and the storetest contract suite.

Item numbers refer to the checklist in `todo/specs/Store specification.md` §15.

## `spike/tx` — items 1, 2, 5 (transactions, CAS, snapshot)

- **§15.1 hooks.** `transaction.GetCurrentTransaction(tctx)` inside
  `p.Transact` returns the live transaction; `OnCommit`/`OnRollback` fire
  exactly once on the matching outcome (`S1 commit path: hooks=[commit]`,
  `S1 rollback path: hooks=[rollback]`). Nested `Transact` joins the outer
  transaction rather than starting a new one, and an inner rollback
  propagates to the outer one (`S5 nested join`, `S5 nested create visible
  after rollback: 0`).
- **§15.2 CAS.** A stale `CollectionUpdate.Version` yields `count=0 rows=0
  err=nil` — **no error**. The correct version yields `count=1 rows=1`.
  `ReadResult.Count` is only reliable when `ReturnDocument` is set, so the
  adapter must translate `count==0` into `ErrConflict` itself rather than
  rely on the error channel. `ModelCollection.Update` already does this: it
  maps the CAS miss to `ErrRecordNotFound`, sets the id filter and
  `ReturnDocument=true` internally, and refreshes its cache on success.
- **§15.5 snapshot.** Read-your-writes hold inside a transaction
  (`S5 read-your-writes inside tx: count=1`); the write is invisible after
  rollback.

## `spike/records` — items 4, 11, 12, 13 (fields, IDs, cache)

- **§15.4 exact numbers.** Free-form record fields do **not** preserve exact
  big integers: input `9007199254740993` comes back as `...992`
  (float64 round-trip). Unknown fields are preserved (`unk: "keep me"`).
  This is why the store keeps free-form JSON as opaque strings rather than
  typed numbers (open decision §17).
- **§15.11 caller-chosen IDs.** A caller-chosen `_id_` is **rejected** unless
  it is a 32-hex dashless UUID (the framework's `isValidID`). So a
  composite `(id, version)` document key is impossible, and the framework's
  model cache cannot be keyed on turn IDs — the workspace needs its own
  domain cache (`S11 caller-chosen id honored: false`,
  `S11 read back by composite id: count=0`).
- **§15.13 cache poisoning.** `ModelCollection.FindByID` serves a document
  that was written inside a transaction that later rolled back: it returned
  `"inside-tx"` where the raw read correctly returned `"before"`. The
  model cache must be disabled in the anansistore adapter; a workspace-level
  cache is the only safe option.
- **§15.12 `ReadAs`.** Projection through `ReadAs` works structurally
  (`1 rows, label="cache"`; the type carries only the projected fields).

## `spike/queries` — items 3, 9 (raw SQL, scan cost)

- **§15.3 raw templates.** Bound `?` parameters work. An injection attempt
  (`x'; DROP TABLE Node;--`) stays data; the table survives. `WITH
  RECURSIVE` is silently dropped by the sqlite executor — the statement runs
  with `err=nil` but returns 0 rows because the executor only reads rows for
  `SELECT`-prefixed SQL. Wrapping the statement in `SELECT * FROM (...)`
  returns all 20 rows. See `docs/upstream.md` §1. The adapter must emit the
  wrapper and detect recursive support with a probe query, treating a
  `WITH`-prefixed success with zero rows as "not supported".
- **§15.9 scan cost.** For 2000 rows: raw `Read` + `Release` averaged
  ~22.4ms; `ModelCollection` bind averaged ~32.1ms — about +9.7ms per 2000
  rows (~4.8µs/row). Hot paths (turn replay, history walks) should use raw
  scans and bind only where a model is actually needed.

## `spike/misc` — items 6, 7, 10 (topic All, keyset, contract slice)

- **§15.6 topic All.** `All([t3,t7,t11])` is implemented as one query per
  topic plus an in-memory intersection over `entity`. On ~10k links the three
  queries + intersection take ~23ms. The intersection is correct (matched
  entities are exactly those carrying all three topics); a `GROUP BY
  entity HAVING COUNT(DISTINCT topic) = n` in SQL would be the next thing to
  try if the fallback ever shows up on a hot path. Deferred to the storetest
  with this rationale.
- **§15.7 keyset paging.** Paging on `_id_` alone (`ORDER BY _id_ ASC`,
  cursor `_id_ > lastID`) is stable under inserts: 202 pages over 10081 rows,
  **0 duplicates**, and a pre-cursor row inserted mid-walk does not shift
  completed pages. Keyset paging is the right shape for the store.
- **§15.10 contract slice on sqlite.** All pass:
  read-your-writes (`1`), commit hooks (`[commit]`), rollback hides the write
  (`rolled-back-write-visible=0`), and a stale-version CAS update returns
  `count=0 err=nil` (which the adapter maps to `ErrConflict`).

## Decisions this pins down

1. Store = sqlite in-memory for spikes and the contract suite; ephemeral is
   unused.
2. Keyset paging on `_id_` (or a dedicated sort key) is the store's paging
   primitive; `Limit`-based offset paging is not needed for the kernel.
3. The anansistore adapter must: map CAS `count==0` to `ErrConflict`, wrap
   recursive SQL in `SELECT * FROM (...)`, and disable the framework model
   cache.
4. Free-form JSON stays opaque strings; big-int precision loss rules out
   storing user numbers as typed fields.
5. Composite `(id, version)` keys are out; the workspace cache is
   domain-owned, keyed on workspace IDs.
