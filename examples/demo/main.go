// Command demo is a terminal chat for manually testing the workspace: pick a
// Gemini model, hold a conversation, and watch the workspace's own events and
// run states while the agent loop does its work.
//
// Usage (from the module root):
//
//	GEMINI_API_KEY=... go run ./examples/demo
//
// The API key is read from GEMINI_API_KEY (GOOGLE_API_KEY also works). A
// missing key only warns: the picker and the conversation shell still run,
// and the first send surfaces the provider failure in the footer, which is
// itself worth seeing.
//
// The demo is deliberately ephemeral — store/memory plus in-memory blobs —
// so a restart starts a fresh workspace. It registers one tool, a shell
// marked RiskConfirm: every command parks the run until the UI is told to
// approve or reject it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asaidimu/workspace"
	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/gemini"
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
	)
	flag.Parse()

	key := firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY"))
	if key == "" {
		fmt.Fprintln(os.Stderr, "demo: no GEMINI_API_KEY or GOOGLE_API_KEY in the environment; "+
			"model selection works but sends will fail at the provider")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := gemini.New(staticCreds(key))
	models, err := client.Models(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: listing models: %v\n", err)
		os.Exit(1)
	}
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "demo: the provider reported no models")
		os.Exit(1)
	}

	ws, err := workspace.Open(ctx, workspace.Config{
		Store:      memory.New(memory.Options{}),
		Blobs:      workspacetest.NewMemoryBlobs(),
		Providers:  []provider.Provider{client},
		Authorizer: kernel.AllowOwner(),
		Tools:      []agent.Tool{shellTool()},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: opening the workspace: %v\n", err)
		os.Exit(1)
	}
	defer ws.Close(context.Background())

	handle := ws.For(workspace.LocalUser(*owner))

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
	})

	p := tea.NewProgram(a, tea.WithAltScreen())
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

// staticCreds is a one-key CredentialSource, the same shape examples/record uses.
type staticCreds string

func (s staticCreds) Credential(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == "" {
		return "", errors.New("no key configured")
	}
	return string(s), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
