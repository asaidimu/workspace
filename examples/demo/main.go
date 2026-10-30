// Command demo is a terminal chat for manually testing the workspace: pick a
// model from Gemini or OpenRouter, hold a conversation, and watch the
// workspace's own events and run states while the agent loop does its work.
//
// Usage (from the module root):
//
//	GEMINI_API_KEY=... go run ./examples/demo
//	OPENROUTER_API_KEY=sk-or-... go run ./examples/demo -provider openrouter
//	OPENROUTER_API_KEY=sk-or-... go run ./examples/demo -model openai/gpt-4o-mini
//	GEMINI_API_KEY=... OPENROUTER_API_KEY=... go run ./examples/demo # both catalogues
//	GEMINI_API_KEY=... go run ./examples/demo -memory                # the old ephemeral mode
//
// Keys come from the environment, one per provider: GEMINI_API_KEY (or
// GOOGLE_API_KEY) and OPENROUTER_API_KEY. With both set the picker lists both
// catalogues and /model filters across them; OpenRouter model IDs look like
// "openai/gpt-4o-mini". -provider gemini or -provider openrouter pins one
// side. A missing key only warns: the picker and the conversation shell still
// run, and the first send surfaces the provider failure in the footer, which
// is itself worth seeing. OpenRouter's catalogue is fetched live from the
// gateway; when that fails the picker falls back to a handful of well-known
// IDs, and any other ID can still be preselected with -model, because the
// adapter serves unknown model IDs permissively.
//
// Conversations persist to SQLite (anansistore) in workspace-demo.db unless
// -memory is given; the newest conversation resumes on start and /sessions
// switches between them. The registered tool is a shell marked RiskConfirm:
// every command parks the run until the UI is told to approve or reject it.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"go.uber.org/zap"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/persistence"
	"github.com/asaidimu/go-anansi/v8/core/query/native"
	sqliteExecutor "github.com/asaidimu/go-anansi/v8/sqlite/executor"
	sqliteQuery "github.com/asaidimu/go-anansi/v8/sqlite/query"

	"github.com/asaidimu/workspace"
	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/gemini"
	"github.com/asaidimu/workspace/provider/openrouter"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/anansistore"
	"github.com/asaidimu/workspace/store/memory"
	"github.com/asaidimu/workspace/workspacetest"
)

func main() {
	var (
		owner        = flag.String("owner", "local", "owner the demo acts as")
		instructions = flag.String("instructions",
			"You are a helpful assistant running inside a terminal demo of the workspace. Be concise. "+
				"You can run shell commands with the shell tool; each command pauses for the user's approval, "+
				"so ask for one when it helps.",
			"system instructions attached to every run")
		db       = flag.String("db", "workspace-demo.db", "SQLite file conversations persist to")
		memory   = flag.Bool("memory", false, "keep everything in memory; nothing survives a restart")
		provFlag = flag.String("provider", "auto", "which provider serves models: auto, gemini or openrouter")
		model    = flag.String("model", "", "model ID to preselect in the picker, e.g. openai/gpt-4o-mini")
	)
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	keys := apiKeys{
		"gemini":     firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY")),
		"openrouter": os.Getenv("OPENROUTER_API_KEY"),
	}

	plan, err := planProviders(*provFlag, keys)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(2)
	}
	for _, n := range plan.notices {
		fmt.Fprintln(os.Stderr, "demo: "+n)
	}

	st, closeStore, err := openStore(*memory, *db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: opening the store: %v\n", err)
		os.Exit(1)
	}
	defer closeStore()

	models, notices := loadModels(ctx, plan.providers)
	for _, n := range notices {
		fmt.Fprintln(os.Stderr, "demo: "+n)
	}
	if len(models) == 0 {
		fmt.Fprintf(os.Stderr, "demo: no provider reported any models\n")
		os.Exit(1)
	}
	if *model != "" {
		models = preselect(models, *model)
	}

	ws, err := workspace.Open(ctx, workspace.Config{
		Store:      st,
		Blobs:      workspacetest.NewMemoryBlobs(),
		Providers:  plan.providers,
		Authorizer: kernel.AllowOwner(),
		Tools:      []agent.Tool{shellTool()},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: opening the workspace: %v\n", err)
		os.Exit(1)
	}
	defer ws.Close(context.Background())

	// Create the collections the store needs; safe to run every start.
	if err := ws.Admin().Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "demo: migrating the store: %v\n", err)
		os.Exit(1)
	}

	handle := ws.For(workspace.LocalUser(*owner))

	// The sessions the picker can resume, newest first. An empty list just
	// means the first pick opens a fresh conversation.
	var sessions []domain.Conversation
	if page, err := handle.Conversations().List(ctx, store.ConversationFilter{}, store.PageRequest{Limit: 50}); err == nil {
		sessions = page.Items
	}

	// Subscribe before the program starts so the first command's events are
	// not missed; the pump blocks on Program.Send until Run consumes.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	envelopes, err := ws.Events().Subscribe(subCtx, kernel.EventFilter{Owner: *owner}, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: subscribing to events: %v\n", err)
		os.Exit(1)
	}

	a := newApp(appConfig{
		handle:       handle,
		owner:        *owner,
		instructions: *instructions,
		models:       models,
		sessions:     sessions,
	})

	p := tea.NewProgram(a, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		for env := range envelopes {
			p.Send(eventMsg{env: env})
		}
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}
}

// apiKeys is the demo's CredentialSource: one environment key per provider
// name, exactly what the adapters ask for on every call. Keys are never
// stored in the workspace and never logged.
type apiKeys map[string]string

func (k apiKeys) Credential(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if key := k[name]; key != "" {
		return key, nil
	}
	return "", fmt.Errorf("no %s API key in the environment", name)
}

// providerPlan is what -provider resolves to: the adapters to register with
// the workspace, plus warnings worth printing before the UI takes over.
type providerPlan struct {
	providers []provider.Provider
	notices   []string
}

// planProviders resolves the -provider flag against the environment. The
// registration order is load-bearing: the workspace routes a model to the
// first provider that claims it, and the OpenRouter adapter permissively
// claims every ID it does not know, so Gemini — which claims only its own
// catalogue — is registered first whenever both are wired. In auto mode a
// provider without a key is skipped rather than wired to fail on every send;
// with no key at all Gemini is wired anyway so the picker still runs, which
// keeps the demo's warn-only stance on missing keys.
func planProviders(choice string, keys apiKeys) (providerPlan, error) {
	switch choice {
	case "gemini":
		plan := providerPlan{providers: []provider.Provider{gemini.New(keys)}}
		if keys["gemini"] == "" {
			plan.notices = append(plan.notices,
				"no GEMINI_API_KEY/GOOGLE_API_KEY; model selection works but sends will fail at the provider")
		}
		return plan, nil
	case "openrouter":
		plan := providerPlan{providers: []provider.Provider{newOpenRouter(keys)}}
		if keys["openrouter"] == "" {
			plan.notices = append(plan.notices,
				"no OPENROUTER_API_KEY; browsing works but sends will fail at the provider")
		}
		return plan, nil
	case "auto":
		var plan providerPlan
		switch {
		case keys["gemini"] != "" && keys["openrouter"] != "":
			plan.providers = []provider.Provider{gemini.New(keys), newOpenRouter(keys)}
		case keys["openrouter"] != "":
			plan.providers = []provider.Provider{newOpenRouter(keys)}
			plan.notices = append(plan.notices,
				"no GEMINI_API_KEY/GOOGLE_API_KEY; only OpenRouter models are listed")
		case keys["gemini"] != "":
			plan.providers = []provider.Provider{gemini.New(keys)}
			plan.notices = append(plan.notices,
				"no OPENROUTER_API_KEY; only Gemini models are listed")
		default:
			plan.providers = []provider.Provider{gemini.New(keys)}
			plan.notices = append(plan.notices,
				"no GEMINI_API_KEY/GOOGLE_API_KEY or OPENROUTER_API_KEY; model selection works but sends will fail at the provider")
		}
		return plan, nil
	default:
		return providerPlan{}, fmt.Errorf("unknown -provider %q (want auto, gemini or openrouter)", choice)
	}
}

// newOpenRouter builds the OpenRouter adapter. OPENROUTER_REFERER and
// OPENROUTER_TITLE, when set, travel as the gateway's optional HTTP-Referer
// and X-Title attribution headers.
func newOpenRouter(keys apiKeys) *openrouter.Client {
	return openrouter.New(keys,
		openrouter.WithAttribution(os.Getenv("OPENROUTER_REFERER"), os.Getenv("OPENROUTER_TITLE")),
	)
}

// loadModels lists every wired provider's catalogue, keeping going when one
// fails so a dead network does not take the picker down with it. When
// OpenRouter's live fetch fails, well-known IDs stand in (see
// openrouterSeed): the adapter serves any OpenRouter model ID permissively,
// so browsing, filtering and sending still behave.
func loadModels(ctx context.Context, ps []provider.Provider) ([]provider.ModelInfo, []string) {
	var (
		models  []provider.ModelInfo
		notices []string
	)
	for _, p := range ps {
		list, err := p.Models(ctx)
		if err != nil {
			notices = append(notices, fmt.Sprintf("listing %s models: %v", p.Name(), err))
			if p.Name() == "openrouter" {
				models = append(models, openrouterSeed()...)
			}
			continue
		}
		models = append(models, list...)
	}
	return models, notices
}

// openrouterSeed is the offline stand-in for the live catalogue: IDs that
// have been stable on the gateway for a long time. Prices are left at zero,
// which the picker renders as context size only. A live fetch replaces these.
func openrouterSeed() []provider.ModelInfo {
	ids := []string{
		"openai/gpt-4o-mini",
		"openai/gpt-4o",
		"anthropic/claude-3.5-sonnet",
		"google/gemini-2.0-flash-001",
		"meta-llama/llama-3.3-70b-instruct",
		"deepseek/deepseek-chat",
	}
	out := make([]provider.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, provider.ModelInfo{ID: id})
	}
	return out
}

// preselect moves -model to the front of the picker so enter chooses it. An
// ID the catalogues do not know is still honoured: the OpenRouter adapter
// claims unknown IDs permissively, so a model the gateway adds tomorrow runs
// without a new release. It renders with a zero context window, which the
// picker shows honestly as "0 ctx".
func preselect(models []provider.ModelInfo, id string) []provider.ModelInfo {
	if id == "" {
		return models
	}
	var chosen provider.ModelInfo
	found := false
	var rest []provider.ModelInfo
	for _, m := range models {
		if m.ID == id {
			chosen = m
			found = true
			continue
		}
		rest = append(rest, m)
	}
	if !found {
		chosen = provider.ModelInfo{ID: id}
	}
	return append([]provider.ModelInfo{chosen}, rest...)
}

// openStore is the demo's storage choice: the ephemeral memory store, or the
// SQLite-backed anansistore that keeps conversations across restarts. The
// wiring is the one anansistore's own tests verify: sqlite executor → native
// interactor → persistence, one connection so SQLite's single writer holds.
func openStore(useMemory bool, path string) (store.Store, func(), error) {
	if useMemory {
		return memory.New(memory.Options{}), func() {}, nil
	}

	logger := zap.NewNop()
	// The document factory is a singleton every anansi persistence op minting
	// documents depends on; configure it once, before anything else.
	if err := data.ConfigureDocumentFactory(data.DocumentFactoryConfig{}, logger); err != nil {
		return nil, nil, fmt.Errorf("configure document factory: %w", err)
	}

	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	exec, err := sqliteExecutor.NewSQLiteExecutor(db, logger)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("sqlite executor: %w", err)
	}
	ix, err := native.NewNativeInteractor(exec, sqliteQuery.NewSQLiteFactory(logger), logger)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("native interactor: %w", err)
	}
	p, err := persistence.NewPersistence(ix, nil, logger, nil)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("persistence: %w", err)
	}

	st, err := anansistore.New(p, anansistore.Options{})
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("anansistore: %w", err)
	}
	return st, func() { _ = db.Close() }, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
