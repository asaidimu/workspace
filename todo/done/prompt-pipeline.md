# Prompt pipeline

## Context

Phase 3 of the delivery plan (spec §17): "registry, pipeline and providers".
This task is the **pipeline** half — `prompt` turns a conversation snapshot
into a `provider.Request` in six deterministic stages (spec §9, lines
502-605), so the same inputs always give a byte-identical prompt. That
determinism is what makes provider switching (scenario S8) safe and what
golden tests assert against.

The kernel (§7) is done and tested at 98.2%; `store/memory` and the
conformance suite are done. `provider/provider.go` already holds §10's
types verbatim (`Request`, `Part`, `Message`, `ToolSpec`, `Usage`,
`FinishReason`, `Response`, `Stream`, `Pricing`, `ModelInfo`, `Source`).

## Important constraints

- **Determinism.** §9: "Assembly is deterministic: sections sort by order
  then ID, maps are read in key order, and nothing uses the clock or
  randomness." No `time.Now`, no `rand`, no map iteration for ordering.
- **Native mode only.** D5: "Native tool calling by default,
  structured-output mode deferred." So the §9 section table's `tools` and
  `blocks` sections appear only in structured mode; the first release
  builds the native path.
- **The TS prompt fixtures do not byte-match, by design.** `testdata/ts-fixtures/prompt.json`
  was dumped from `@asaidimu/utils-workspace@9.0.0` and encodes that
  library's *structured-output* system prompt (the "WORKSPACE OPERATING
  SYSTEM" / "BLOCK TYPES & USAGE RULES" text). D5 defers that mode, so
  byte-identity with those strings is not the goal. The fixtures stay
  useful as a reference for **transcript mapping** semantics (case
  `transcript-mapping`). Golden tests for this package are derived from
  the spec's own §9 section table and budget rules, not from the TS
  system-prompt strings.
- **`prompt` may import:** stdlib, `domain`, `provider`, `store` (the port,
  for `store.ContentRef` in `Scored`). Not anansi, not an adapter —
  enforced by `boundary/boundary_test.go` (§5 rule 1-2).
- Test doubles exist: `workspacetest.FakeClock`, `SeqIDs`, `ScriptedProvider`.

## Steps

- [*] Write `prompt/types.go`: the plain structs §9 names (`Snapshot`,
  `Candidate`, `Summary`, `Input`, `Diagnostic`) plus the spec's declared
  types verbatim (`Stage`, `Build`, `Section`, `Budget`, `RetrievalQuery`,
  `Scored`, `Retriever`, `Compactor`, `Tokenizer`, `Pipeline`). `Input` was
  extended with the fields the stages need (`Tools []provider.ToolSpec`,
  `ContextWindow`, `MaxOutputTokens`, `Tokenizer`, `Retriever`, `Compactor`,
  `Policy`, `Structured`, `OutputSchema`, `RetrievalLimit`).
- [*] Write `prompt/stages.go`: `DefaultPipeline` plus the six stages in
  order — snapshot, select, resolve, compact, assemble, map. `Build` seeds a
  `*Build` from `Input` and carries `Input` to stages in an unexported field,
  because `Stage.Run` only receives `*Build` while the exported shape of
  `Build` must stay exactly as §9 declares it.
- [*] Budget: `Window = ContextWindow - ReservedOutput`, reserved output
  being `MaxOutputTokens` or 10% of the window. Fixed sections costed first
  and an overflow fails with `INVALID`. Transcript fitted **newest first** by
  pairing each tool call with its result into a unit, so a pair is kept or
  dropped together. Elided turns go to the `Compactor` when one is
  configured, and its summary becomes a leading system turn.
- [*] Defaults: `Estimator` (bytes/4 rounded up, `token.approximate`
  diagnostic) and `TopicRetriever` (overlap, then recency, limit default 20).
- [*] Section assembly per §9's table, sorted by Order then ID. **Resolved:**
  a key that is both attached and listed as a preference resolves as a
  preference — `specificity()` ranks preference above attachment above
  retrieval, because a preference key's purpose is to be a preference.
  Within one tier the higher version wins, and any key offered by more than
  one distinct (kind, version, score) records a `content.conflict`.
- [*] Map stage: blocks → `provider.Part`s including `tool_call` →
  `ToolCallPart` and `tool_result` → `ToolResultPart` (the agent loop needs
  this), with `image`/`file` downgraded to a marker when no `Source` is
  resolvable. Native mode (default) puts tool specs on the request;
  structured mode attaches the output schema and describes tools in a
  section.
- [*] Tests: determinism, section ordering, fixed-section overflow, newest-
  first elision, tool-pair atomicity, compaction, estimator diagnostics,
  resolve precedence/tombstone/conflict/unresolved, retriever ranking and
  limits, cancelled contexts, and stage names. 91.1% coverage (spec §16
  wants 90%). Green under `go test -race -count=2 -shuffle=on`.
- [*] Update this file's statuses as work lands; do not delete steps.

## Files

- Modify: `prompt/prompt.go`, `prompt/types.go` (stubs exist).
- New: `prompt/pipeline.go`, `prompt/*_test.go`.
- Reference: `todo/specs/Workspace specification.md` §9; `provider/provider.go` (§10
  types); `domain/domain.go` (`Turn`, `Block`, `Content`); `store/store.go`
  (`ContentRef`).

## Schema duplication (cross-reference — see also `content/jsonschema.go`)

The anansi-to-JSON-Schema converter landed here, in the phase after this
one, because `agent.NewTool` needed to describe a tool's arguments from a Go
type (todo/done/agent-loop.md). Rather than write a second schema system, the
route goes through anansi: reflect the Go type into a
`definition.Schema`, then convert that to JSON Schema. Both halves live in
`content` — the one place §5 rule 1 allows anansi's schema packages — as
`content/schema_for.go` (`SchemaFor`) and `content/jsonschema.go`
(`JSONSchema`). The `content` package comment carries the full note; the
summary is that the **converter is the durable half** (anansi is the single
schema authority) and the **reflector is the disposable half**, a stand-in
for anansi's own `data.ExtractDTOSchemaDirect` which lives in `core/data`,
out of reach of every core package. When that extractor moves somewhere
importable, delete `SchemaFor` and keep `JSONSchema`. This is the
duplication the note is loud about: schema description is anansi's job, and
the reflector duplicates a sliver of it until upstream makes the rest
reachable.

## Decisions pinned

- Errors use anansi `*common.SystemError` via `kernel.WrapError`/`IsCode`,
  the eight codes from §7/§15. A budget overflow is `INVALID`.
- The TS prompt fixtures are a transcript-mapping reference only; goldens
  are spec-derived (see constraint above).
- No OTel, no tracing dep in the core (kernel gap note, same reasoning).
- Do not run `go mod tidy`: it drops the pinned `blobs`/`hestia` requires.
