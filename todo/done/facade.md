# Facade wiring + milestone (end-to-end conversation)

## Context

Delivery milestone from `todo/specs/Sources and agent brief.md`: "test a full conversation
in-process via `store/memory` + `ScriptedProvider`". Everything under it is
done: `content` (schema chain, 96%+), `prompt` (91.1%), `kernel` (98.2%),
`store/memory` + `storestoretest` conformance, `agent` (tool/registry/step/
manager, 91.8%). The root package `workspace.go` is still all stubs, so nothing
can be opened yet.

This task wires the one place adapters meet (`todo/specs/Workspace specification.md`
§5 rule 4: only `workspace.Open` imports an adapter) and proves the facade with
the milestone test.

## Decisions pinned here

- **Commands dispatch through the kernel**, not direct store writes: the Handle
  methods build a command struct, `kernel.Register` at `Open`, and each method
  calls `kernel.Dispatch`. That buys authorization, validation, logging,
  error-code mapping and post-commit events for free and keeps §7's rule that
  reads/writes go through the same chain. Reads dispatch too (a handler can
  return a value and emit no events) so the authorizer cannot be bypassed.
- **`agent.History` is implemented by the facade over scoped repos**
  (`history.go`). `Read` assembles `prompt.Snapshot`; `Append` commits the turn
  and advances the conversation's head revision inside the handler's
  `store.Atomic`.
- **Known deviation, do not "fix" silently**: §11 says a turn commit and the
  run checkpoint must share one `Atomic`. Today the turn is committed by
  `History.Append` (inside its command's transaction) and the run checkpoint by
  the manager one step later — two commits, correct *ordering* (turn before
  tools, results before next step) but not one atomic unit. Making them one
  transaction requires the run's cost/pending state to be computable at append
  time, which the manager only knows after the step returns. Leave the ordering
  guarantee, record the gap here, and let a future decision pick either a
  context hook that carries the checkpoint into the append or a spec change.
- **`RunOptions.Tools`** added to `agent.RunOptions` and threaded into
  `StepOptions.Tools` in the manager's loop, so `run.start` can name tools.
- **run.* commands bypass kernel dispatch.** `run.start/get/cancel/approve` in
  `handle.go` call the manager and owner-scoped repos directly (manager still
  emits the §13 event names `run.started`/`run.cancelled`/`run.resumed`).
  Reason: a command handler runs inside `store.Atomic`, and a *scoped* write
  issued from within an open transaction is staged on the committed datastore
  while the transaction commits its own clone — the nested write is silently
  lost. Dispatching run.* would drop `run.started`. The workaround is recorded
  on the `ownerField` doc comment in `commands.go`.
- **`memory.Store.Scoped` is a session, not a snapshot** (bug found by the
  approval test): `Atomic` commits by swapping in a fresh `*datastore`
  pointer, so a scoped handle created before any commit kept reading/writing
  the orphaned copy — the manager's `Checkpoint`/`SetStatus(waiting)` became
  invisible while the park event still fired. Fixed in `store/memory/memory.go`:
  `scopedRepos` holds no `ds`; every accessor resolves `st.ds` under the store
  mutex. Repos fetched per call (all facade paths, `reposSystem()`) are live.
  The residual hazard — an in-place scoped write landing *during* an open
  transaction is clobbered at commit (pointer staleness check cannot see it) —
  needs a store version counter; tracked separately, it is what forces the
  run.* bypass above.
- **Manager redrive after loop exit** (`agent/manager.go` `redrive`): a parked
  loop returning and `Approve`'s `drive()` race on `m.driving[runID]` — if
  Approve wins the flag is still set, drive skips, and the run hangs until
  recovery. The loop's defer now clears the flag then re-drives any run that
  is `running` (the resumed state). Surfaced as a rare `-count=3` flake in
  `TestApprovalParksTheRunAndApproveResumesIt`; fixed and hammered 300x.
- **Deferred**: `Config.Registry` is accepted but not yet applied to append
  (block-type validation lands with the content wiring phase) and `Pricer` is
  nil (needs a configured FX rate for micro-KES).

## Steps

- [*] `workspace.go` `Open`: requires Store, Blobs, Providers, Authorizer
      (spec §5/§15); defaults Clock/IDs/Logger (`kernel.DefaultClock`,
      `kernel.DefaultIDs`, `WithTxTimeout` from Options.TxTimeout),
      `prompt.NewPipeline`, `agent.NewRegistry` seeded with `cfg.Tools`, facade
      `history`, `agent.NewStepper` (returns the `Runner` that satisfies both
      Stepper and Resolver) and `agent.NewManager` (kernel bus as Emitter).
      Duplicate tool names → `CONFLICT`; cancelled ctx → `CANCELED`.
- [*] `workspace.go` `For`, `Events` (buffered bus sized by
      `Options.EventBuffer`), `Close` (no owned resources; does not close the
      caller's store or stop in-flight runs).
- [*] `commands.go`: all §13 command types with value-receiver
      `CommandName()`/`Target()`, event types, `ownerField` (owner stamped from
      the principal — handlers receive no principal), `registerCommands()`.
      Reads dispatch too so the authorizer cannot be bypassed.
- [*] `handle.go`: `Handle` + sub-interfaces; generic `dispatch` (FORBIDDEN
      when `p.Owner` is empty), `runs` bypasses dispatch (see decisions),
      `blobs.Upload` stores bytes before registering metadata (dedupe by
      checksum → same ID).
- [*] `history.go`: `Read` = one read-only Atomic → `prompt.Snapshot` (chain
      reversed to oldest-first; role/settings NotFound tolerated);
      `Append` = one Atomic → `commitTurn` with idempotent receipt
      (`turnReceiptTTL` 24h) and conditional `SetHead` (`linkToHead=true` for
      append/switch, `false` for edit so a new version keeps its parent).
- [*] `workspacetest/blobs.go`: `MemoryBlobs` sha256 content-addressed
      double (Put/Open/Stat/Delete/List).
- [*] `workspace_test.go` (package `workspace_test`, root): milestone and
      beyond — `TestFullConversationInProcess`, `TestRunWithAToolRoundTrip`
      (chain user/call/result/final, echo tool ran once, second request carries
      the result), `TestApprovalResumesAParkedRun` (RiskConfirm parks without
      executing, Approve resumes to success), `TestEditBranchAndFork`,
      `TestHandleErrorsCarryCodes`, `TestRolesContentAndBlobs`,
      `TestAdminReportsHealth`, `TestOpenValidatesConfiguration`. All spins are
      `runtime.Gosched()` + 5s `time.Now()` deadline; no `time.Sleep`.
      NOT covered: §11's crash-mid-step tool re-run — it needs fault injection
      (a `FaultyStore` / `Harness.Restart` at a precise point), which lands
      with the fault-injection phase, not here.
- [*] Suite green: `gofmt -l .`, `go build ./...`, `go vet ./...`,
      `go test -race -count=3 -shuffle=on ./...`. Coverage: root facade
      **75.1%**, kernel 95.9%, agent 91.8%, prompt 91.1% (90% bar holds for
      kernel/agent).

## Files

- Modify: `workspace.go`, `inputs.go` (if DTO tweaks fall out), `agent/manager.go`
  (`RunOptions.Tools`), `todo/done/agent-loop.md` (close the re-run gap note).
- Create: `commands.go`, `handle.go`, `history.go`, `workspace_test.go`,
  `workspacetest/blobs.go`, this file.

## Status

- [*] Milestone delivered: full conversation in-process via `store/memory` +
      `ScriptedProvider`, plus tool round-trip, approval flow, branch/fork,
      error codes, admin and Open validation. Two real bugs found and fixed by
      the tests (memory Scoped orphaning, manager drive/park race) — both
      recorded under decisions above.
