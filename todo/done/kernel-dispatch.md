# Kernel dispatch pipeline

## Context

The kernel is the command dispatcher: every change to workspace state passes
through it. `todo/specs/Workspace specification.md` §7 (lines ~206-371) defines the
seven-step path a command takes:

1. Build an `Envelope` (principal, tracing IDs, optional idempotency key).
2. Run the middleware chain, outermost first: panic recovery, tracing,
   logging, validation.
3. Authorize; the default is deny, and opening a workspace without an
   authorizer is an error.
4. Admit through the keyed serializer (FIFO per conversation, or per owner
   for owner-level commands). Never held across a provider call.
5. `store.Atomic` runs the handler, reserving the idempotency key first when
   one is given.
6. The handler returns a result plus events, held until commit.
7. After commit, events reach the bus; after rollback, compensation hooks run.

Errors are already done: `kernel/errors.go` uses anansi `*common.SystemError`
(spec §7 errors bullet, §15 cross-cutting). `IsCode`/`CodeOf` walk the chain
and check nested `Issue`s; `wrapStore`/`wrapContext` classify store and
context errors into the eight codes `NOT_FOUND`…`INTERNAL`, keeping the
original on the cause chain. Covered 100% in `kernel/errors_test.go`.

## What exists today

`kernel/kernel.go` holds the type declarations verbatim from spec §7:
`Command`, `Event`, `Principal`, `Envelope`, `Target`, `Authorizer`,
`Handler[C,R]`, `Next`, `Middleware`, `EventEnvelope`, `EventFilter`, `Bus`.

The pipeline itself is implemented and tested: `kernel/api.go` (real `Kernel`,
options, `New`, default clock/UUIDv7 IDs), `kernel/bus.go`,
`kernel/serializer.go`, `kernel/authorizer.go`, `kernel/dispatch.go`
(`Register`/`Dispatch`/`execute`/default chain). Tests:
`kernel/{errors,serializer,bus,dispatch,api}_test.go`, 98.2% coverage.

The store port is complete and passing the conformance suite:
`store/store.go` (§6 interfaces verbatim), `store/types.go`,
`store/memory` (TxSerializable, CAS on stale rev → `ErrConflict`, keyset
paging, run claim protocol), `store/storetest`.

Test doubles exist in `workspacetest/`: `FakeClock`, `SeqIDs`, `EventLog`
(waits on events, never sleeps), `ScriptedProvider`, `Harness`.

## Steps

- [*] Implement the keyed serializer: per-key FIFO, context-aware waiting,
  never held across a provider call. Correctness across processes comes from
  the store's CAS, not this lock. (`kernel/serializer.go`, tested in
  `kernel/serializer_test.go`: mutual exclusion, context cancel while queued
  removes the waiter, FIFO order, different keys never block.)
- [*] Implement the in-process `Bus`: ordered per conversation, at most once,
  buffered channels, disconnect a slow subscriber after a final overflow
  event so it re-syncs by querying. (`kernel/bus.go`; `Overflow` is exported
  so a host can detect a missed-events gap. Tested in `kernel/bus_test.go`.)
- [*] Implement `AllowOwner()` — permits every command whose principal has an
  owner. Resource-level ownership is the store's owner-scoped repos, a second
  independent wall (spec §7). (`kernel/authorizer.go`.)
- [*] Implement `New(s store.Store, opts ...Option) (*Kernel, error)`. Errors
  if no store, no authorizer, nil clock, nil ID generator, or a
  non-positive receipt TTL. (`kernel/api.go`.)
- [*] Implement `Register[C,R]` and `Dispatch[C,R]` with the seven steps and
  the default chain recovery → logging → validation → host middleware →
  execute. Idempotency: Reserve → handle → Complete inside one
  `store.Atomic`; a completed receipt short-circuits to the stored result and
  rolls back this attempt's reservation, publishing nothing. A reservation
  with no result is `CONFLICT` retryable. (`kernel/dispatch.go`.)
- [*] Map store sentinels to kernel codes via `wrapStore`, and set
  `Operation` (command name) and `Path` (target ID) on the returned
  `SystemError`. (`kernel/errors.go`; authorizer refusals become `FORBIDDEN`
  unless they already carry a code — `classifyAuthz`.)
- [*] Tests: one per pipeline step, plus idempotent re-dispatch returning the
  stored result, rollback publishing nothing and leaving no writes,
  per-conversation ordering, panic recovery, correlation-ID propagation.
  Kernel coverage is 98.2% (spec §16 needs 90%). All green under
  `go test -race -count=3 -shuffle=on`.
- [*] Update this file's statuses as work lands; do not delete steps.

## Gaps resolved by decision

- **Tracing.** Spec §7 lists tracing in the default chain, but no OTel
  dependency is pinned, and pulling OTel into an embeddable library would
  force it on every host. Decision: keep it out; `WithMiddleware` is the
  documented extension point. Host middleware is applied outside `execute`
  (dispatch.go buildChain), so a host's tracing middleware correctly
  captures serializer queue-wait plus handler time. When OTel is pinned, add
  a default `tracing` middleware between `recovery` and `logMiddleware`,
  matching §7's stated order.
- **Compensation hooks on rollback.** §7 step 7 says "after rollback,
  compensation hooks run", and the migration table (line 1396) frames it as
  the replacement for the TS original's sagas, covering **external effects**
  only — durable writes are already covered by the transaction. §7's type
  list defines no kernel-owned hook type, so the kernel owns none.
  Decision: compensation is a per-handler concern through the store port's
  `Tx.AfterCommit`/`Tx.AfterRollback` (store/store.go:49-50), which handlers
  can register because they receive `tx`. Proven end-to-end through Dispatch
  in `kernel/dispatch_test.go`: `TestHandlerCompensationRunsOnRollback`,
  `...OnCommit`, and `TestCompensationDoesNotDuplicateAcrossReplay` (a failed
  idempotent attempt rolls back its reservation, so a retry re-runs the
  effect and confirms it — no duplicate refund).

## Files

- Done: `kernel/api.go`, `kernel/bus.go`, `kernel/serializer.go`,
  `kernel/authorizer.go`, `kernel/dispatch.go`, `kernel/errors.go`, plus
  `kernel/{errors,serializer,bus,dispatch,api}_test.go`.
- Untouched: `kernel/kernel.go` (declarations as written from §7).
- Reference: `todo/specs/Workspace specification.md` §7, `store/store.go` (§6 port),
  `store/memory/` (the reference adapter), `workspacetest/` (doubles).

## Decisions already pinned

- Errors are anansi `*common.SystemError`, not a bespoke type (spec §7, §15).
- The kernel may import anansi `core/common` only; enforced by
  `boundary.TestKernelOnlyCommonAnansi`. `core/common` is pure stdlib.
- Store CAS on a stale revision returns `count=0 err=nil` from anansi; the
  adapter maps that to `ErrConflict` (docs/spike.md §15.2, §15.10).
- The ephemeral anansi backend is not used anywhere (docs/upstream.md).
- Do not run `go mod tidy`: it drops the pinned `blobs`/`hestia` requires.
