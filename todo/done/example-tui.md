# Example TUI for manual workspace testing

## Context

The user wants a runnable example that exercises the workspace end to end in a
terminal, using charmbracelet UI libraries. Minimum scope: (1) model selection
— Gemini for now, (2) basic conversation (user message → agent run → reply).
This doubles as a manual smoke test of the Phase 0–4 stack through the real
facade (`workspace.Open`), the real Gemini adapter and the real agent loop,
with no scripted provider.

Key facts already verified (any agent can rely on these):

- Wiring: `workspace.Open(ctx, workspace.Config{Store, Blobs, Providers,
  Authorizer: kernel.AllowOwner()})`, then `ws.For(workspace.LocalUser(owner))`.
  Store: `memory.New(memory.Options{})`. Blobs: only implementation outside
  tests is `workspacetest.NewMemoryBlobs()` (`blob.Store` has no other impl;
  `github.com/asaidimu/blobs` is an unused indirect dep).
- Gemini: `gemini.New(creds)` where creds is a `provider.CredentialSource`
  (`Credential(ctx, name) (string, error)`); static key pattern lives in
  `cmd/record/main.go` (env: `GEMINI_API_KEY` or `GOOGLE_API_KEY`).
  `geminiClient.Models(ctx)` is a static catalogue, no network
  (gemini-2.5-pro, gemini-2.5-flash).
- Conversation flow: `Conversations().Create` → `Turns().Append(user turn,
  workspace.Text(msg))` → `Runs().Start(convID, workspace.StartRun{Model:
  model, Instructions: ...})`. `Start` returns immediately; `m.drive` runs
  `go m.loop(runID)` in the background.
- Run completion emits NO event (only `run.started`, `run.approval_requested`,
  `run.resumed`, `run.cancelled` exist) → the UI must poll
  `Runs().Get(runID)` until `Status.IsTerminal()`, then reload the transcript
  with `Turns().Chain(convID, store.ChainOptions{})` (oldest first; agent
  turns are committed by the loop's History, not through dispatch).
- Events: `ws.Events().Subscribe(ctx, kernel.EventFilter{Owner: owner}, 0)`
  (0 buffer → Options.EventBuffer default 64); envelope has `.Name` and
  `.Event`. Forward to the tea program for a visible event log.
- Boundary test (`boundary/boundary_test.go`) only binds `corePkgs`; a new
  `cmd/` package may import adapters and third-party UI libs freely.
- Deps: pin `github.com/charmbracelet/bubbletea@v1.3.10`,
  `bubbles@v0.21.1`, `lipgloss@v1.1.0` (v1 APIs; no `/v2` module paths).
  Use `go get <mod>@<ver>` — **never `go mod tidy`**. (`go mod download` for
  `github.com/asaidimu/blobs` and `github.com/asaidimu/hestia` was needed to
  satisfy `go list -m -versions`; go.sum entries only, no version changes.)
- No tools are registered in the demo → no approval parking in normal use.

## Steps

- [*] Add charmbracelet deps (bubbletea v1.3.10, bubbles v0.21.1, lipgloss v1.1.0) via `go get`, go.mod/go.sum only
  - **Files:** `go.mod`, `go.sum`
- [*] `cmd/demo/main.go`: flags (`-owner`, `-instructions`), env key check (warn, don't hard-fail, so the run-failure path is also testable), workspace wiring (memory store, workspacetest memory blobs, gemini provider, kernel.AllowOwner), fetch model catalogue, subscribe to events and pump envelopes into the tea program, run program in alt-screen, cancel ctx on exit
  - **Files:** `cmd/demo/main.go`
- [*] `cmd/demo/app.go`: tea.Model with two phases — model picker (custom cursor menu over `provider.ModelInfo`, up/down/enter) and chat (bubbles/textinput + viewport transcript, spinner + status footer, event-log footer). On send: append user turn, start run, poll `Runs().Get` every ~250ms while non-terminal; on terminal reload chain, render reply or failure; ctrl+c cancels an active run then quits
  - **Files:** `cmd/demo/app.go`
- [*] Extract turn text robustly (`Value map[string]any{"text":...}`, plain string, or `Raw json.RawMessage`) since the memory store round-trips blocks
  - **Files:** `cmd/demo/app.go`
- [*] Verify: `gofmt -l .`, `go build ./...`, `go vet ./...`, full `go test -race -count=3 -shuffle=on ./...` (boundary + all suites) — demo itself has no tests but must not break any gate
  - **Verification:** full gate command above
- [*] Update this file's statuses and record anything learned the next agent needs

## Done notes

- `cmd/demo/main.go` + `cmd/demo/app.go` + `cmd/demo/app_test.go`; run with
  `GEMINI_API_KEY=... go run ./cmd/demo`.
- Verified: `gofmt -l .` clean, `go build ./...`, `go vet ./...`,
  `go test ./... -count=1` all green (incl. boundary). `go test -race
  -count=3 -shuffle=on ./...` was started and then aborted by the user; rerun
  before release. Headless tests in `cmd/demo/app_test.go` cover picker →
  chat → send → poll → render, the failed-run footer, and event display.
- Sandbox limitation discovered: bubbletea receives NO key events on a
  script(1)/pty.fork pty here — reproduced with a minimal tea program (no
  KeyMsg at all, even for `cat` the pty works). So the TUI was smoke-tested
  only to the first rendered frame (model picker renders correctly); manual
  interactive testing must happen in a real terminal.
- Demo is ephemeral (store/memory + workspacetest memory blobs) and registers
  no tools, so no approval flow. It polls `Runs().Get` because terminal run
  states emit no event (only run.started/approval_requested/resumed/
  cancelled exist).
