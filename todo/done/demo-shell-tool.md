# Demo: shell tool with approval

Status: `[*]` done. Created 2026-10-30.

## Why this exists

The demo registered no tools, so the agent loop's approval path (spec
section 11) was unreachable in the only user-facing surface. The user asked
for a shell tool that must be approved before a command runs.

## What landed

- `examples/demo/shell.go`: `shellTool()`, an `agent.NewTool` with
  `Risk: agent.RiskConfirm`, a 60 s timeout, 64 KiB output cap, and a
  hand-written JSON Schema (the derived form carries no field descriptions).
  It runs `sh -c <command>` with an optional `workdir`, captures stdout,
  stderr and the exit code, and treats a non-zero exit as a normal result so
  the model can read the failure.
- `examples/demo/main.go`: registers the tool on `workspace.Config.Tools` and
  the default instructions mention it and the approval pause.
- `examples/demo/app.go`: `agent.ApprovalRequested` events queue an
  `approvalRequest`; the composer is replaced by a red "permission required"
  panel showing the exact command (arguments come from the committed
  assistant turn, found by call ID); `y` approves, `n` rejects, `esc` cancels;
  the panel is fixed at 8 rows so the transcript does not jump. Keystrokes are
  swallowed while the panel is up. Status line says "waiting for permission".
  Pending approvals are cleared when the run ends or a new message is sent.
- `examples/demo/render.go`: shell-aware tool rendering — `⚙ shell $ cmd` for
  the call and `↳ $ cmd exit N` plus capped stdout/stderr code blocks for the
  result; `asObject` normalizes nested raw JSON from either store;
  `findToolCall`/`toolArguments` read a call's arguments from the chain.
- Tests: `TestShellApprovalRunsTheCommand` (panel, `y`, stdout `hi`, follow-up
  reply, two provider calls, layout 30×100),
  `TestShellRejectionTellsTheModel` (`n`, error result mentions rejection),
  plus the test workspace now registers the tool.

## Notes

- Polling deliberately does **not** reload the transcript while a run is
  running: `Turns().Chain` raced the run's own store writes and produced
  intermittent `CONFLICT` failures. It reloads when the run parks (so the
  pending call is visible) and when it reaches a terminal state.
- Gemini rejects `"type": ["string","null"]` in tool schemas with
  INVALID_ARGUMENT. The shell schema now says `"string"` for `workdir`, and the
  Gemini adapter flattens nullable type arrays (tools and output schemas)
  before they reach the wire; see `provider/gemini/schema_test.go`.
- No sandbox: the command runs with the demo user's privileges. Approval is
  the only guard, which is acceptable for a local developer demo and is
  stated in `shell.go` and the tool description.
