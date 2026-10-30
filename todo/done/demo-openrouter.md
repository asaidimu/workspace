# Demo: OpenRouter provider support

- [*] Wire `provider/openrouter` into `examples/demo/main.go`
  - **Context:** The demo is the manual test bench for the workspace, but it only
    ever wired the Gemini adapter, so OpenRouter models (and the openrouter
    package's live priced catalogue) could not be exercised end to end.
  - **Details:**
    - Replace `staticCreds` with `apiKeys`, a per-provider env key map
      (`gemini` → `GEMINI_API_KEY`/`GOOGLE_API_KEY`, `openrouter` →
      `OPENROUTER_API_KEY`). The adapters ask for the key by provider name on
      every call, so one source serves both.
    - `-provider auto|gemini|openrouter` (default `auto`) decides what is
      wired. Auto wires every provider that has a key; with no key at all it
      falls back to the historic keyless Gemini setup so the picker still runs
      (missing keys only warn — unchanged stance).
    - Registration order is load-bearing: the workspace routes a model to the
      first provider that claims it, and OpenRouter permissively claims any ID
      it does not know, so Gemini is always registered first when both run.
    - `-model <id>` preselects a model in the picker (moved to front). An ID
      the catalogues do not know is still honoured — the OpenRouter adapter
      serves unknown IDs permissively.
    - Catalogue loading keeps going when one provider fails; a failed
      OpenRouter fetch falls back to a handful of well-known IDs so the picker
      is never empty offline.
    - `OPENROUTER_REFERER` / `OPENROUTER_TITLE` map to the adapter's optional
      attribution headers.
  - **Files:** `examples/demo/main.go`, `examples/demo/main_test.go` (new),
    `todo/demo-openrouter.md` (this file, moved to `todo/done/` when closed).
- [*] Gates: gofmt, `go build ./...`, `go vet ./...`,
  `go test -race -shuffle=on ./...`
- [*] Bundle the work for download: commit the pending interaction upgrade and
  this change, `git bundle create`, verify, and prove a fresh clone builds.
  - **Note:** the repo rule "code is not committed" is overridden here — the
    user explicitly asked for a git bundle to test with, and a bundle carries
    commits.
