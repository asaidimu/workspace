# Demo: slash commands

Status: `[*]` done. Created 2026-10-30.

## Why this exists

The opencode-style demo has keybindings but no command surface: starting a
new conversation, changing models or retrying all require keys or a restart.
opencode exposes these as slash commands with an inline suggestion menu. This
brief adds the same shape to `examples/demo`.

## Design

- Commands: `/help` (show the palette), `/new` (fresh conversation on the
  same model), `/model` (back to the picker), `/retry` (retry the last failed
  run), `/quit`.
- Typing `/` opens a suggestion list directly above the composer, filtered by
  the text after the slash. `↑`/`↓` move the selection, `tab` completes it,
  `enter` runs the selected command (or the typed one when it matches).
- Commands that need an idle workspace (`/new`, `/model`) refuse while a run
  is in flight with a footer notice; `/quit` always works.
- Unknown commands set a footer notice instead of being sent to the model.
- The palette takes its rows from the transcript height, like the composer.

## Steps

- [*] `examples/demo/commands.go`: command table (name, description, run),
      matching, palette rendering.
- [*] `examples/demo/app.go`: palette state (`cmdCursor`), enter/tab/up/down
      handling, `paletteHeight` in layout, insert the palette above the
      composer, update the footer hint.
- [*] `/new` and `/model` share the conversation-creation path with the
      picker; both are idle-only.
- [*] Tests: palette filters, tab completes, enter runs, `/new` rotates the
      conversation, unknown command sets a notice, `/quit` quits.
- [*] Gate: `gofmt -l .`, `go build ./...`, `go vet ./...`,
      `go test -race -count=3 -shuffle=on ./...`.

## What landed

- `commands.go`: `/help`, `/new`, `/model`, `/retry`, `/quit`; prefix matching,
  bordered suggestion palette above the composer, exact-name dispatch with the
  remainder kept as args.
- `app.go`: `cmdCursor`, up/down/tab/esc handling while the palette is open,
  palette rows subtracted from the transcript height, slash text routed
  locally instead of to the model, footer hint now mentions `/`.
- `/new` rotates the conversation through the same `createConversation` path
  as the picker; `/model` reopens the picker on the current model; both refuse
  while a run is in flight. `/help` reopens the full palette. Unknown commands
  set a footer notice and are never sent.
- Tests: palette contents/filter/tab/enter, `/new` rotates the conversation,
  unknown command sends nothing, `/quit` quits, palette layout stays 30 lines
  at 100x30.

## Constraints

- No new dependencies; no command needs arguments yet (the parser keeps the
  remainder so arguments can be added later).
- The transcript is store-backed: `/help` reuses the palette rather than
  injecting fake turns.
