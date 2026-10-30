# Todo index

This folder is the durable plan for the workspace library. Each file is a
self-contained brief: enough context for another agent to pick it up without
re-reading the whole conversation.

## Layout

```
todo/
  index.md               this index
  specs/                 source specifications (read these first)
    Workspace specification.md
    Store specification.md
    Sources and agent brief.md
  done/                  completed phase briefs, kept for history
```

New work goes at the top level of `todo/`; when every step is `[*]`, move the
file into `todo/done/` and leave the final notes intact.

## Status identifiers

- `[ ]` planned
- `[-]` in progress
- `[*]` done
- `[=]` skipped (say why)
- `[X]` blocked (say by what)

## Current state

- Phases 0-4 are complete and gated: fixtures, store ports + memory +
  anansistore, kernel, prompt pipeline, content registry, agent loop, facade,
  boundary tests, providers and adapters, failure injection, and the
  `examples/demo` TUI.
- The full gate is green, including `go test -race -count=3 -shuffle=on
  ./...` across all packages.
- `provider/openrouter` wraps the OpenAI-compatible wire with the gateway's
  base URL, attribution headers and a live `/models` catalogue (real context
  windows and prices); unknown model IDs fall back to permissive capabilities.
- Provider/tool hardening: the Gemini adapter flattens nullable `type` arrays
  (`["string","null"]`) in tool and output schemas, which Gemini's Schema proto
  rejects with INVALID_ARGUMENT.
- Run terminal transitions are serialised in the manager (`commitTerminal`), so
  Cancel racing the loop announces `run.cancelled` exactly once.
- `store-owner-scope.md` is the one known spec deviation: by-ID `Get` is not
  owner-filtered in either store (Store spec rule 10); blob resolution guards
  itself, other callers do not.
- `done/blocks-hybrid.md` closed the Go-vs-TS prompt/registry gaps: hybrid
  block markup, registry-driven prompt, blob resolution, effects dispatch and
  settings/constraints.
- `done/demo-slash-commands.md` adds `/help`, `/new`, `/model`, `/retry` and
  `/quit` with a filtered palette above the composer.
- `done/demo-shell-tool.md` gives the demo a RiskConfirm shell tool and the
  approval panel that gates it.
- `done/example-opencode-ui.md` rebuilt the demo's TUI in an opencode-like layout
  (header, gutter messages, markdown-lite, multiline composer, thinking row).
- Not yet planned in a brief: Phase 5 (public API polish, reference CLI,
  hestia adapter) and Phase 6 (license, security review, benchmarks, docs,
  v0.1 tag) from spec section 17.

## Repo rules that affect this folder

- Do not commit unless the user explicitly asks.
- Never run `go mod tidy`; the dependency pins are deliberate.
- After each phase run: `gofmt -l .`, `go build ./...`, `go vet ./...`,
  `go test -race -count=3 -shuffle=on ./...`.
