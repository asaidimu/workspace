# Demo: opencode-style terminal UI

Status: `[*]` done. Created 2026-10-30.

## Why this exists

`examples/demo` is the only user-facing surface of the library. Its chat
screen is a plain viewport plus a one-line textinput: every block type except
`text` used to render as `[type]`, the composer cannot grow, there is no
visible "working" state while a run is in flight, and the model picker is a
bare list. The user asked for a UI closer to the `opencode` CLI.

This is a demo, not a shipped command: it stays in `examples/demo`, uses only
the library's public API, and must not grow features the library lacks
(streaming is not available through the agent loop, so a "thinking" row
stands in for token streaming).

## Design

- **Layout:** header bar (wordmark, model, short conversation id) → scrolling
  transcript → rounded composer (multiline, prompt `❯`) → footer with status,
  key hints and the last workspace event.
- **Messages:** each turn is a left-gutter block (`▌`) with a role label
  (`you`, `agent`, `tool`) and a body. Assistant prose goes through a small
  markdown renderer: headings, bullets, blockquotes, rules, fenced code, and
  inline `code`/`**bold**`. Non-text blocks reuse the existing readable
  renderers (`document`, `json`, `thinking`, `unknown`).
- **Composer:** bubbles `textarea`, 1–6 lines, `enter` sends, `ctrl+j` inserts
  a newline, grows as the text grows and shrinks the transcript to match.
  While a run is in flight the border dims and `enter` is ignored.
- **Picker:** centered rounded panel with the model list, pricing/context
  hints, and a highlighted row.
- **Status:** spinner + "working" while a run is in flight; `● ready` when
  idle; `✗ failed` with `ctrl+r` retry when a run failed. Errors and notices
  appear above the composer, not only in the footer.
- **Theme:** fixed truecolor palette in `theme.go` (blue user, purple agent,
  amber tool, red error, green success) with a muted panel colour.

## Steps

- [*] `examples/demo/theme.go`: palette, styles (header, panel, gutter,
      labels, code, status), width/pad helpers.
- [*] `examples/demo/render.go`: move `turnBody`, `textFrom`, `blockObject`,
      `blockString`, `documentText`, `jsonText`, `unknownText` out of
      `app.go`; add `renderMarkdown`, `styleInline`, `wrapText`, and
      `renderTurn` (gutter + label + body + tool rows).
- [*] `examples/demo/app.go`: replace `textinput` with `textarea`; new
      `headerView`, `composerView`, `footerView`, `thinkingRow`; layout math
      in `syncLayout`; keep `phase`, `cursor`, `modelID`, `convID`, `runID`,
      `status`, `failure`, `notice`, `transcript` fields the tests use.
- [*] Keymap: `enter` send, `ctrl+j` newline, `ctrl+r` retry, `esc`
      cancel/quit, `ctrl+u`/`ctrl+d`/`pgup`/`pgdn` scroll, picker
      `↑`/`↓`/`j`/`k`/`enter`/`q`.
- [*] Tests: markdown renderer (heading, bullet, fence), composer newline,
      thinking row while a run is in flight, picker still selects, failed run
      still shows its failure, event still reaches the footer.
- [*] Gate: `gofmt -l .`, `go build ./...`, `go vet ./...`,
      `go test -race -count=1 -shuffle=on ./...`.

## What landed

- `theme.go` (palette + styles + width helpers), `render.go` (markdown-lite,
  gutter message rendering, block renderers) and a rewritten `app.go`
  (textarea composer, header/composer/footer, layout math, thinking row).
- Keys: `enter` send, `ctrl+j` newline, `ctrl+r` retry, `esc` cancel/quit,
  `ctrl+u`/`ctrl+d`/`pgup`/`pgdn` scroll; picker keeps `↑`/`↓`/`j`/`k`.
- Tests added: markdown formatting, composer growth, thinking row, layout
  fills the window, plus the existing picker/send/retry/event contracts.
- Deliberately left out: streaming (agent loop is request/response), session
  sidebar, command palette, themes beyond the fixed palette.

## Constraints

- Do not add new module dependencies. `bubbles`, `bubbletea` and `lipgloss`
  are already pinned; glamour is not available, hence the small markdown
  renderer.
- No streaming: the agent loop is request/response, so the in-flight row is a
  spinner, not partial text.
- Keep `go run ./examples/demo` working and the existing tests' contract
  (`workspace demo`, role `agent`, `conv <id>`, `run.started` in the footer).
