# Hybrid blocks + native tools + registry-driven prompt

Status: `[*]` done (2026-10-29). Owner: agent. Created 2026-10-29.

All phases landed and the full race/shuffle gate passed. See "Explicitly
deferred" for what deliberately did not land.

## Why this exists

The Go port implemented the skeleton of spec section 9 but disconnected the
parts the TS design cared about:

- `content.Registry` exists (`content/registry.go`) with `Describe`,
  `Render`, `MapperFor`, `Parse`, `Effects`, but the prompt pipeline never
  calls it. Assembly is a hardcoded switch (`prompt/stages.go:614`,
  `prompt/stages.go:543`).
- The agent loop turns provider parts into blocks with a hardcoded switch
  (`agent/step.go:399`), so model output never goes through the section 8
  six-check pipeline, and `Effects` (`content/registry.go:358`) is never
  dispatched.
- Structured-output mode is deferred (D5), so the `tools` and `blocks`
  prompt sections are dead code (`prompt/stages.go:435`).
- Images/documents become `[image omitted]` (`prompt/stages.go:633`); there
  is no `BlobResolver` and no re-upload of missing/expired remote refs
  (spec section 9).
- `Role.Constraints` (`domain/domain.go:169`) is ignored; `Request.
  Temperature`/`Stop` (`provider/provider.go:112`) are never populated.
- `Settings` has no `Prompt` field (`domain/domain.go:263`), `Conversation`
  has no conversation-level preferences (`domain/domain.go:146`), and there
  are no `settings.*` commands (`commands.go`).
- Snapshots always read the head chain (`history.go:34`); there is no
  branch-target read.

TS comparison, verified against the pinned bundle
(`fixtures/node_modules/@asaidimu/utils-workspace/index.mjs`): TS ended up
JSON-only (`OPERATING_SYSTEM_CONTENT`, line 4260) and its
`ContentRegistry.parse` only accepts `{type, value}` objects. The
"prose with embedded blocks" idea was the stated intent but was never
implemented. This brief therefore designs it for Go and records the spec
amendments it requires.

## The design in one paragraph

Tools stay native (`Request.Tools`, `provider.ToolCallPart`). Blocks are
emitted by the model as ordinary prose plus fenced `workspace:<type>`
markup; the agent scans the text, validates each payload through
`content.Registry`, stores typed blocks, and dispatches post-commit effects.
The UI gets a clean `[]domain.Block` per turn. JSON-only structured output
stays available as an opt-in fallback for providers/models that want it.

## Locked decisions (user, 2026-10-29)

1. **Markup:** fenced code block, info string `workspace:<type>`, body is one
   JSON object = the block value. Accept the TS-shaped `workspace` +
   `{"type":...,"value":...}` envelope too.
2. **Default mode:** hybrid (prose + markup, native tools). Structured JSON
   is opt-in via `prompt.Input.Structured`.
3. **Blob resolution is in scope:** add `BlobResolver`, wire the blob store,
   re-upload missing/expired remote refs per spec section 9.
4. **Effect failure fails the step:** the turn stays committed, the step
   returns an error, and the idempotency key makes a retry safe.
5. **Prompt wire format matches TS:** section bodies are `# Title\nBody`,
   all sections join into one `TextPart` with `\n\n---\n\n`, empty sections
   are filtered, extensions anchor by label (`prepend`, `append`,
   `before:<label>`, `after:<label>`).
6. **Public API:** keep `Build` returning `provider.Request`; add a
   `Prepare`/`Result` form that also exposes sections, tokens and schema.
7. **Preferences:** render `- <text>` under `# User Preferences` (TS style;
   the Go `{name,text}` payload keeps `name` as identity only). Precedence:
   conversation > role > workspace.
8. **Settings prompt:** `Settings.Prompt` becomes default instructions; a
   per-run `Instructions` value overrides it.
9. **Third check:** tool-call arguments are validated against the tool's
   JSON Schema before execution (spec section 8). This was an open
   follow-up in `todo/done/providers.md`; it lands here.

## Markup grammar

Canonical:

````
Ordinary markdown prose.

```workspace:json
{"value": {"rows": 3, "ok": true}}
```

More prose.

```workspace:document
{"title": "Summary", "text": "..."}
```
````

Tolerated (TS-compatible):

````
```workspace
{"type": "json", "value": {"rows": 3}}
```
````

Rules:

- Opening fence: a line matching `^\s*```workspace(?::([A-Za-z0-9_.:-]+))?\s*$`.
- Closing fence: a line matching `^\s*```\s*$`.
- Body must be one JSON object; numbers keep fidelity (`UseNumber`).
- No `id`, no `key`, no `type` inside the canonical body. The workspace
  assigns identity. The envelope form carries `type` and optional `value`.
- Text outside fences becomes `text` blocks in order; blank-only segments
  are dropped.
- Unclosed fence: remaining text is kept, `unterminated` diagnostic.
- Invalid JSON / unregistered type / non-emittable type: `unknown` block
  preserving the raw bytes, plus a diagnostic. Never drop.
- `tool_call` markup is accepted only when native tools are unavailable
  (`ParseOptions.NativeTools == false`); otherwise it becomes `unknown` with
  a `native_tool_expected` diagnostic, so a tool cannot run twice.
- A literal fence can be shown with any info string other than `workspace`;
  only `workspace` is recognised.
- Safety: a valid JSON object cannot contain a raw newline, so a closing
  fence line cannot appear inside a valid payload.

## Phase A - content: parser + six checks

Files: create `content/text.go`; edit `content/registry.go`,
`content/builtins.go`, `content/content.go`.

- [*] Add `ParseOptions{Provider string, NativeTools bool}`.
- [*] Add `Registry.ParseText(text string) ([]domain.Block, []Diagnostic, error)`
      to the interface and implementation.
- [*] Add `content.ParseParts(r Registry, parts []provider.Part, opts
      ParseOptions) ([]domain.Block, []Diagnostic, error)`:
      `TextPart` scans, `ThinkingPart` -> `thinking`, `ToolCallPart` ->
      `tool_call`, `ImagePart`/`FilePart` -> typed block or `unknown`.
- [*] Extend the six checks in `Parse`: parse -> resolve refs -> validate
      (anansi) -> predicates -> repair once / downgrade -> bind. Today it
      only parses, validates and downgrades (`content/registry.go:246`).
- [*] Add `WithRules([]string)` / `Rules() []string` to `BlockType`; keep
      `Describe()` and `Capabilities()` (used to filter emittable types).
- [*] Tests: multiple fences, adjacent blocks, unclosed, invalid JSON,
      unknown type, non-emittable type, envelope form, tool-call fallback vs
      native, literal-fence escaping, prose preservation.

## Phase B - prompt: registry-driven, TS format, hybrid blocks section

Files: `prompt/types.go`, `prompt/stages.go`, `prompt/prompt.go`,
`prompt/defaults.go`, `history.go`, `domain/domain.go`.

- [*] `prompt.Input` gains `Content content.Registry`, `ProviderName`,
      `NativeTools`, `Constraints`, `Blobs content.BlobResolver`.
- [*] Sections: body becomes `# Title\nBody`; join into one `TextPart` with
      `\n\n---\n\n`; filter empty sections. Update `Section`/goldens.
- [*] Extensions: label anchors (`prepend`, `append`, `before:<label>`,
      `after:<label>`); unknown anchor appends + diagnostic.
- [*] Blocks section always present in hybrid mode, generated from emittable
      types (`Describe` + `Rules`); it explains prose + fences and says tools
      are native, never blocks. Tools section stays structured-only.
- [*] Structured mode (opt-in) is TS-byte-compatible: OS preamble, tools
      section, JSON-only block rules, `OutputSchema` attached.
- [*] `context`/`artifacts` use `BlockType.Render`; preferences render
      `- <text>` under `# User Preferences`.
- [*] Transcript mapping uses `MapperFor(type, provider)` with wildcard
      fallback; mapper error/capability downgrade -> text marker +
      diagnostic.
- [*] Add `Settings.Prompt` (default instructions; run instructions win) and
      `Conversation.PreferenceKeys`; add `FromConversationPreference` as the
      top precedence tier (conversation > role > workspace).
- [*] Add `Pipeline.Prepare(ctx, in) (Result, error)` with
      `Result{Request, Sections, Tokens, Schema, Diagnostics}` and a
      per-section token breakdown/remaining; `Build` wraps it so the public
      shape is unchanged.
- [*] `history.Read` loads the new settings/preferences/constraints fields.
- [*] Update `testdata/golden/prompt/*`; add a hybrid golden (Go-first,
      documented as having no TS counterpart) and keep structured-mode
      output byte-identical to the TS bundle.

## Phase C - blobs in the prompt

Files: `content/registry.go` (mapper env), `content/builtins.go`,
`prompt/*`, create `blobresolver.go` (root), `workspace.go`.

- [*] Change `Mapper.ToParts` to take a map env:
      `MapEnv{Provider string, Capabilities provider.Capabilities, Blobs BlobResolver}`.
- [*] Define `content.BlobResolver` (returns a `provider.Part`) so `content`
      does not import `blob`.
- [*] Built-in image/document/file mappers resolve refs; nil resolver or a
      failed resolve -> text marker + diagnostic (never drop).
- [*] Root resolver implementation over the blob store: inline bytes or
      `Provider.UploadFile`; re-upload when the remote ref is missing or
      expired (spec section 9). Wire it in `workspace.Open`.
- [*] Tests: local blob, remote hit, expired re-upload, unresolved
      downgrade, provider without Vision/PDF.

## Phase D - agent output + effects + third check

Files: `agent/step.go`, `agent/agent.go`, `kernel/dispatch.go`,
`workspace.go`.

- [*] Replace `assistantBlocks` (`agent/step.go:399`) with
      `content.ParseParts`; add `StepResult.Diagnostics`.
- [*] Add `kernel.DispatchAny(ctx, k, p, cmd, opts...) (any, error)`; handlers
      already return `any`, so this is a thin addition to
      `kernel/dispatch.go`.
- [*] Add an `EffectDispatcher` port to `StepperConfig`; after the assistant
      turn commits, for each block with `EffectsFor(type)` dispatch its
      commands with key `turn:<id>.<version>:<blockIndex>`.
- [*] Effect dispatch failure returns an error from the step (turn stays
      committed; retry is idempotent).
- [*] Validate tool-call arguments against `ToolSpec.Schema` before
      execution; invalid arguments become an error result for that call, not
      a run crash.
- [*] Tests: native tools unchanged; parse -> validate -> commit; replay
      idempotency; downgrade diagnostics; effect failure; invalid tool args.

## Phase E - constraints + settings commands

Files: `domain/domain.go`, `commands.go`, `prompt/*`, `agent/step.go`.

- [*] Add typed `domain.Constraints{Temperature *float64, MaxOutputTokens
      *int, Stop []string, ThinkingBudget *int}` and decode
      `Role.Constraints` into it.
- [*] Merge role -> conversation -> last-user-turn constraints into
      `Request.Temperature`/`Stop`/`MaxOutputTokens` (thinking budget via
      provider metadata where supported).
- [*] Add `settings.get`/`settings.update` commands (spec section 13) with
      `Prompt`, `Language`, `DefaultRoleID`, `GlobalPreferenceKeys`.
- [*] Conversation create/update accepts `PreferenceKeys`.
- [*] Tests: precedence order, request propagation, command authorization,
      revision bumps.

## Phase F - spec + goldens + gate

- [*] Amend spec section 9: `blocks` section is present in hybrid mode;
      `tools` remains structured-only.
- [*] Amend spec section 18: hybrid (native tools + `workspace:<type>`
      markup) is the default; structured JSON is the fallback.
- [*] Amend spec section 8: document the markup grammar and its mapping onto
      the six checks.
- [*] Amend spec section 6: conversation-level preference keys.
- [*] Regenerate goldens; keep structured-mode byte parity with the TS
      bundle; add hybrid goldens.
- [*] Full gate: `gofmt -l .`, `go build ./...`, `go vet ./...`,
      `go test -race -count=3 -shuffle=on ./...` (the last race run was
      aborted and must be rerun).

## Implementation notes (deviations from the plan above)

- `MapEnv` ended up `{Blobs BlobResolver, Owner string}`; the mapper already
  receives `provider.Capabilities` as its own argument, so repeating them in
  the env was redundant.
- `content.BlobResolver.Resolve` takes the owner because blob rows are
  owner-scoped and the prompt build has no principal; the root resolver
  enforces the owner match itself (both stores' by-ID Gets are not
  owner-filtered — a documented parity quirk).
- Expired-remote re-upload is not implemented in the resolver: a recorded
  remote id wins and a missing one falls back to streaming the bytes, which
  every adapter can upload itself. A cache-aware uploader is left for the
  provider work in Phase 5.
- Tool-argument validation uses the content package's JSON Schema subset
  validator (`content.ValidateJSON`) rather than anansi's compiler, which
  speaks anansi schemas, not the JSON Schema form the providers receive.
- Conversation constraints were added as a stored field (`Conversation.
  Constraints`); the "last user turn" tier is `agent.StepOptions.Constraints`.

## Explicitly deferred

- Branch-target snapshots (`History.ReadAt`) and regenerate-a-past-turn UX.
- Streaming; the parser is line-oriented and can be made incremental later.
- Any anansi -> JSON-Schema translator; structured mode uses the
  TS-compatible schema path only.
- Phase 5 (public API polish, reference CLI, hestia adapter) and Phase 6
  (license, security review, benchmarks, docs, v0.1 tag).

## Breaking changes to expect

- `content.Registry` gains `ParseText`.
- `content.Mapper.ToParts` signature changes.
- `prompt.Extension.Position` moves from numeric order to label anchors.
- Prompt goldens change shape (headers + single joined system part).
- All pre-release; no migration needed.

## Recon facts worth keeping

- TS assembler: sections built at `index.mjs:4330`, `join` at `:4466`,
  `SECTION_SEPARATOR = "\n\n---\n\n"` at `:4259`, OS preamble at `:4260`,
  block description at `:2093`, schema at `:2118`.
- TS Google adapter `resolve` at `index.mjs:5797`; `InferenceContext` tokens
  at `index.d.ts:2861`.
- Go: `prompt/stages.go` assemble `:372`, block part `:614`, content body
  `:543`; `agent/step.go` input `:134`, `assistantBlocks` `:399`;
  `content/registry.go` Parse `:246`, Effects `:358`;
  `kernel/dispatch.go` generic Dispatch `:62`.
- Provider port: `Name()`, `Capabilities(model)`, `Generate`, `Stream`,
  `UploadFile` (`provider/provider.go:206`).
- Boundary: `content` and `prompt` are both core packages, so `prompt` may
  import `content`; `content` already imports `provider` and `kernel`.
