# Agent loop and tools

## Context

Phase 4 of the delivery plan (spec §17) and the second half of the
kernel-adjacent work: `agent` is the run manager, the step primitive and the
tool registry (spec §11, lines 793-927). Its types are already declared
verbatim in `agent/agent.go`; only `NewTool` is stubbed.

The kernel (§7) is done at 98.2% and the prompt pipeline (§9) is done at
91.1%. `provider/provider.go` holds §10's types. `store/memory` plus the
conformance suite are done.

The milestone this unlocks: a full conversation driven in-process — kernel
commands persist turns, `prompt` builds the request, a `ScriptedProvider`
supplies the model output, and the step commits the assistant turn before
running tools.

## Important constraints

- **Commit order (§11).** The assistant turn, tool calls included, is
  committed **before** any tool runs, and tool results are committed before
  the next step. One `Atomic` call appends the turn and checkpoints the run
  with a compare-and-swap on the step counter. A restart repeats at most the
  step in flight.
- **Tools run at least once.** A call with no committed result is re-run
  after a restart, using the call ID as the idempotency key. A tool not
  marked idempotent that may already have run is not re-run automatically;
  the run pauses for approval, flagged as possibly executed.
- **Narrowed identity.** Tools run under an agent principal whose scopes
  are the intersection of the user's scopes and the role's allowed tools. No
  tool can widen them.
- **Limits.** Steps (25), deadline (10 minutes), cost cap in micro-KES.
  Exceeding any one fails the run with a specific code; all committed work
  stays in the history.
- **Determinism in tests.** No sleeps, no real clock, no network, no real
  DB. `workspacetest.FakeClock`, `SeqIDs`, `ScriptedProvider` exist.
- **`agent` may import:** stdlib, `domain`, `kernel`, `provider`, `prompt`,
  `store` (the port). Not anansi, not an adapter — enforced by
  `boundary/boundary_test.go`.
- **`NewTool`'s schema route (open decision 8, resolved).** The derivation is
  Go type -> anansi `definition.Schema` -> JSON Schema, with **both halves
  in `content`** (`content.SchemaFor` reflects, `content.JSONSchema`
  converts), so anansi stays confined to `content` and `kernel` as §5 rule 1
  requires. `agent` imports only `content` — it never names a `definition`
  type. This route came with a loud note about the duplication it introduces
  (anansi already owns schema description): the converter is the durable half,
  the reflector the disposable half standing in for anansi's off-limits
  `data.ExtractDTOSchemaDirect`. See the package comments in
  `content/jsonschema.go` and `content/schema_for.go`, and
  `todo/done/prompt-pipeline.md` § "Schema duplication".
  `NewTool` returns `Tool`, not `(Tool, error)`, so an unsupported input
  type panics at registration — a programmer mistake caught at startup,
  matching `kernel.Register`'s nil-handler panic.

## Steps

- [*] `agent/tool.go`: implement `NewTool[In, Out]` — derive a JSON Schema
  from `In` by reflection (`json` tags, structs, slices, maps, scalars,
  pointers as optional, nested structs), unmarshal the call arguments into
  `In`, run the function, marshal `Out` into `ToolResult.Output`, and
  truncate to `ToolSpec.MaxOutput` with a flag when it is exceeded.
  **Done:** `agent/tool.go` chains `content.JSONSchemaFor` (which is
  `SchemaFor` + `JSONSchema`); derivation, truncation, name-mismatch and
  invalid-args paths covered by `agent/tool_test.go` (9 green) and
  `content/schema_for_test.go` (12 green), all under `-race`.
- [*] `agent/registry.go`: in-memory `Registry` — `Register` rejects
  duplicates and empty names, `Lookup` by name, `Specs(p)` returns only
  tools whose `Scopes` the principal holds (narrowed identity: a tool can
  never widen what the principal already has).
  **Done:** `NewRegistry()` returns a RWMutex-guarded map; `Specs` sorts by
  name so the pipeline's request is byte-stable; a tool is visible only when
  the principal holds *every* scope it declares. 6 tests green under
  `-race -shuffle`.
- [*] `agent/step.go`: the `Stepper` — one model call plus the execution of
  the tool calls it asks for. Build the request with `prompt`, call the
  provider, commit the assistant turn (and a run checkpoint) in one
  `Atomic`, then execute tools and commit their results before returning.
  Tools execute under a narrowed `kernel.Principal`.
  **Done:** committal goes through a `History` port (`Read`/`Append`) rather
  than the store directly, so the facade can route appends through the
  kernel (authorization, validation, events, idempotency) while tests drive
  it directly — `agent` never touches `TurnRepo`. The narrowed principal is
  computed **once per step** (user scopes ∩ the scopes the exposed tools
  ask for) and is an `AgentPrincipal`; `RiskConfirm` calls pend instead of
  running, and a step where everything is pending commits no result turn.
  Parallelism is bounded by `MaxParallel` (default 4) with results ordered
  by call index; cancellation after the assistant turn discards only the
  results. 13 tests, 94.4% coverage, green under `-race -count=3 -shuffle`.
- [*] `agent/manager.go`: the `Manager` — run states (queued, running,
  waiting for approval, succeeded, failed, cancelled), step/deadline/cost
  caps, `RiskConfirm` approval flow emitting `run.approval_requested`,
  `Cancel` cancelling a run-owned context (never the caller's), and
  `Recover` claiming stale runs.
  **Done:** `NewManager` returns `RunManager` = `Manager` + `Stepper` +
  `Checkpointer`, because §11 says "the manager implements both interfaces".
  One loop goroutine per run drives it over a context derived from nothing
  the caller controls; a 10-second heartbeat ticker keeps a run blocked in a
  long provider call from looking abandoned. `waiting` needed a port method
  that did not exist: `RunRepo.SetStatus` (CAS on the step, same style as
  `Checkpoint`) plus `domain.RunWaiting`, both added with a `storetest`
  conformance case, so a parked run is neither claimable nor listed as
  active. Events are exactly the names §13 gives (`run.started`,
  `run.approval_requested`, `run.resumed`, `run.cancelled`) and are emitted
  only after the state they report is committed, via a new `kernel.Emit`
  for in-workspace producers that are not command handlers. Lapsed
  approvals become rejections; a rejected or lapsed call commits an error
  result through `CallResolver.Resolve` so the model adapts.
- [*] Tests: schema derivation for the supported kinds, scope filtering,
  the commit-before-tools ordering, step and cost caps failing with specific
  codes, approval pausing and resuming, and cancellation discarding
  uncommitted partial text. Kernel
  coverage target from §16 is 90% for the kernel; agent gets the same bar.
  **Done: 91.8% coverage**, green under `go test -race -count=3 -shuffle=on`.
  39 tests across `tool_test.go` (10), `registry_test.go` (6),
  `step_test.go` (16), `manager_test.go` (17). Two tests pin the ordering
   rule against a single shared timeline rather than two logs concatenated
   after the fact, and the parallelism test would deadlock rather than pass
   by luck if tools stopped running concurrently. **Not yet covered:** the
   stepper-level idempotency-key re-run (§11 "using the call ID as the
   idempotency key") — crash recovery at the manager level is now pinned by
   the fault-injection step below, but the call-ID-keyed re-run itself has
   no dedicated test; recorded in `todo/done/facade.md`.
- [*] Phase 4 gate (spec §17, line 1337): failure-injection tests at every
  checkpoint, alongside the approval-flow and step/cost/deadline-cap tests
  that share the gate.
  **Done:** `agent/checkpoint_inject_test.go` (8 green) drives a
  `faultStore` decorator over `store/memory` that intercepts exactly one of
  the loop's four persistence points — `SetStatus(running)` (claim),
  `Checkpoint` (per-step CAS), `SetStatus(waiting)` (park), `Finish` — in
  two modes: **transient** (the call errors once, then heals: the run ends
  `failed` with the injected error in `Failure`, committed turns preserved;
  a transient `Finish` error is swallowed by `finish()` and the loop's
  `redrive` completes the run with exactly one repeated step) and **crash**
  (the call blocks until the test releases it and never reaches the store,
  and `Heartbeat` freezes so the run goes stale when the `FakeClock`
  advances). Crash recovery runs on a **second `NewManager` over the healthy
  inner store** — the crashed manager still holds its `driving[runID]` flag,
  so its own `Recover` could never re-drive the run. Each crash test pins
  the invariant: no committed turn lost, at most the in-flight step
  repeated, worker becomes `recovery-worker`.
- [*] `agent/manager.go` re-park fix: a crash between the checkpoint that
  folds pending calls into `run.State` and the `SetStatus(waiting)` write
  left the run `running` with committed pending calls; the loop would have
  stepped past a call nobody approved (and, at the next checkpoint, wiped
  `st.Pending` via `st.Pending = nil`). The loop now re-parks any run that
  enters with committed pending calls: `SetStatus(waiting)` on the current
  step, re-emit `run.approval_requested` per call, return.
  **Done:** added at the top of `loop()` after the queued-claim; pinned by
  `TestCrashWhileParkingIsRecoveredByReparkingAndApprovalRunsTheCall`, which
  fails (times out on `waiting`) with the fix removed — verified by
  temporarily reverting it. Also corrects the multi-pending case: approving
  one of several parked calls now re-parks for the rest instead of silently
  dropping them at the next checkpoint's `st.Pending = nil`.
- [*] Update this file's statuses as work lands; do not delete steps.

## Files

- Modify: `agent/agent.go` (`NewTool` stub removed; its doc carries the
  schema-route decision). Also `domain/domain.go` (`RunWaiting`,
  `RunStatus.IsTerminal`), `store/store.go` (`RunRepo.SetStatus`),
  `store/memory/repos.go` (its implementation), `store/storetest/` (run
  conformance), `kernel/api.go` (`Emit`, `kernel/errors.go` (`FromStore`).
- New: `agent/tool.go`, `agent/registry.go`, `agent/step.go`,
  `agent/manager.go`, `agent/*_test.go`.
- Reference: `todo/specs/Workspace specification.md` §11; `kernel/kernel.go` (§7
  types), `kernel/dispatch.go` (`Register`/`Dispatch`); `prompt` (§9,
  `DefaultPipeline`); `provider/provider.go` (§10); `store/store.go` (§6,
  `RunRepo`); `workspacetest/` (doubles).

## Decisions pinned

- Compensation for external effects stays per-handler via
  `store.Tx.AfterCommit`/`AfterRollback`; the kernel owns no hook type
  (kernel gap note).
- No tracing dep in the core; a host adds it with `kernel.WithMiddleware`.
- `NewTool` panics on an unsupported input type rather than returning an
  error, because the signature is `Tool` and a bad tool is a startup bug.
- Do not run `go mod tidy`: it drops the pinned `blobs`/`hestia` requires.
