# Demo: opencode interaction pass (live transcript, queue, dialogs, SQLite sessions)

Status: `[*]` done. Created 2026-10-30.

## Why this exists

The user compared `examples/demo` with the `opencode` TUI and chose an
"interaction pass" after a gap analysis. The prior pass
(`todo/done/example-opencode-ui.md`) fixed layout and rendering shape; this
one closes the behavioural gaps. Scope confirmed with the user:

1. Live transcript — tool turns appear while a run is in flight.
2. Message queueing — Enter during a run queues and auto-sends later.
3. Collapsible detail — tool output and thinking collapsed by default,
   `ctrl+o` toggles; a tool call without its result shows as running.
4. Mouse wheel scrolling.
5. Overlay dialogs — model picker, sessions picker, and a real `/help`
   replace the full-screen phase flip and the "type /" hack.
6. SQLite sessions — `store/anansistore` behind the demo so conversations
   survive restarts, with resume of the newest conversation on start.
7. glamour markdown — real CommonMark + chroma syntax highlighting. This
   deliberately breaks the "no new module dependencies" rule at the user's
   request; `go mod tidy` stays forbidden (it would drop the blobs/hestia
   pins).
8. opencode-neutral theme — near-black panels, dim gray text, one warm
   amber accent, minimal borders.

Not in scope: token streaming (agent loop is request/response), branching UI
(edit/fork/regenerate), `@`-file mentions, attachments, themes picker.

## Design

- **Live updates:** the event pump already delivers every envelope; the
  handler now reloads the chain when `env.Name == "turn.appended"` and
  `env.ConversationID == a.convID` (EventEnvelope carries ConversationID).
  Run status still comes from the poll loop.
- **Queue:** Enter with a run in flight appends to `a.queued` and clears the
  composer. When the poll sees `RunSucceeded`, the first queued message is
  sent immediately (FIFO). If the run failed, the first queued message goes
  back into the composer for editing. Queue rows render above the composer.
- **Detail:** `a.expand` flips collapsed/expanded for tool output and
  thinking blocks; rendering knows which tool calls have results, so an
  unmatched call under an active run renders `◌ running`.
- **Dialogs:** a `a.dialog` string routes keys; the dialog box composites
  over the chat view by line splicing (no new overlay dependency). Dialogs
  have type-to-prefix filters. The model dialog only switches `modelID`;
  conversation creation stays with `/new` and first launch.
- **Persistence:** `-db` flag (default `workspace-demo.db`, `-memory` keeps
  the old behaviour). Wiring copies `anansistore_test.go`:
  ConfigureDocumentFactory → sql.Open (1 conn, WAL) → SQLiteExecutor →
  NativeInteractor → Persistence → anansistore.New → Open →
  `ws.Admin().Migrate(ctx)`. Newest conversation auto-resumes after the
  model pick. Blobs stay in memory: uploads are not a demo feature, so
  documents with blob refs break across restarts (known, acceptable).
- **Footer:** line 1 status + hints (unchanged contract), line 2 keeps the
  last event (tests assert `run.started` appears) and gains session usage —
  tokens and cost summed from agent turns' `domain.Usage`.
- **Markdown cache:** glamour is too slow to run per keystroke; renders are
  cached by turn ID + version + width + expand flag, bounded at 500 entries.

## Steps

- [*] `examples/demo/theme.go`: opencode-neutral palette and styles.
- [*] `examples/demo/markdown.go`: custom glamour StyleConfig, per-width
      renderers, render cache, `renderMarkdown` keeps its signature.
- [*] `examples/demo/render.go`: demoted user turns (`❯`), label-only agent
      turns, status-row tool rendering with collapse and running state.
- [*] `examples/demo/app.go`: queue, live reload, mouse, usage footer,
      dialog state and keys, layout accounting for the queue rows.
- [*] `examples/demo/dialogs.go`: overlay composite, model/sessions/help
      dialog views.
- [*] `examples/demo/commands.go`: `/help`, `/model`, `/sessions` open
      dialogs; palette keeps its filtering contract.
- [*] `examples/demo/main.go`: SQLite wiring, migration, resume list, mouse
      option.
- [*] `examples/demo/app_test.go`: keep the surviving contracts, update
      `/model` and markdown expectations, add queue/live-reload/mouse/
      collapse/sessions tests.
- [*] Gate: `gofmt -l .`, `go build ./...`, `go vet ./...`,
      `go test -race -count=3 -shuffle=on ./...`.

## Constraints

- Public library API only; the demo stays in `examples/demo`.
- No `go mod tidy`; glamour was added with `go get` and existing pins stay.
- No new dependencies beyond glamour (chroma/goldmark arrive with it).
- Keep the test contract words: `workspace demo`, `agent`, `conv <id>`,
  `run.started` in the footer, `thinking` row while a run is in flight,
  `permission required` approval panel.
