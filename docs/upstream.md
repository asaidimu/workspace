# Upstream findings (go-anansi v8.10.2)

Notes on issues found in the pinned `github.com/asaidimu/go-anansi/v8@v8.10.2`
while spiking the workspace store. Each is reproduced by a program under
`spike/`; run with `ANANSI_ENV=development go run ./spike/<name>`.

## 1. `ExecuteQuery` drops rows for `WITH`-prefixed SQL (sqlite)

**Where:** `sqlite/executor/executor.go:152` (module cache).

The sqlite executor only reads result rows when the template SQL begins with
`SELECT`. A template that begins with `WITH` / `WITH RECURSIVE` executes
successfully (`err == nil`) but returns `Count == 0` — no error, silent empty
result. Anything the CTE computes is discarded.

**Workaround:** wrap the statement so it starts with `SELECT`:

```sql
SELECT * FROM (WITH RECURSIVE walk(...) AS (...) SELECT ... FROM walk)
```

With the wrapper the same recursive query returns all 20 rows of the fixture
chain. `spike/queries` prints both the bare (`count=0`) and wrapped
(`count=20`) results side by side.

**Impact on workspace:** the ancestor walk (`todo/specs/Workspace specification.md`)
needs recursive CTEs over the `Link` junction collection. The adapter must
emit the `SELECT * FROM (...)` wrapper until this is fixed upstream. The
recursive probe in `spike/queries` also shows the adapter can *detect*
support by running a probe query and treating a `WITH`-prefixed success with
zero rows as "not supported".

**Capability note:** `RecursiveQueries` should be detected at runtime the same
way, not assumed. On the pinned version the probe succeeds only with the
wrapper; a backend that returns an error for `WITH` entirely is distinguishable
from one that silently returns zero rows.

## 2. `OrderBy` projection bug — ephemeral backend only (not sqlite)

**Status: not a blocker.** During an earlier spike against the ephemeral
backend, DSL reads that included any `OrderBy` returned incomplete user
fields (only the first order field survived; two or more order fields
returned none), while `Count` stayed correct. Reproduced shapes
(`spike/sqlitemisc` runs the same matrix on sqlite):

| query | ephemeral | sqlite |
|---|---|---|
| 1–2 filters, no order | all 5 fields | all 5 fields |
| 1 filter + 1 `OrderBy` | only `created` | all 5 fields |
| 1 filter + 2 `OrderBy` | none | all 5 fields |
| no filter + 2 `OrderBy` | none | all 5 fields |

SQLite is the production store and returns every field in every shape, so
this does not affect the workspace store. It is recorded only because it
cost time while investigating the keyset cursor; if the ephemeral backend is
ever used for the storetest contract suite, this needs an upstream fix (or
the suite must avoid multi-field `OrderBy` on ephemeral).

**Decision:** the ephemeral backend is not used anywhere in the workspace
store. All spikes and the storetest contract suite run against sqlite
in-memory (`anansi.Playground` with `DBPath: ":memory:"`).
