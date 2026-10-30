package main

import (
	"context"
	"errors"
	"testing"

	"github.com/asaidimu/workspace/provider"
)

func TestAPIKeysServeEachProvider(t *testing.T) {
	keys := apiKeys{"gemini": "g-key", "openrouter": "or-key"}

	if got, err := keys.Credential(context.Background(), "gemini"); err != nil || got != "g-key" {
		t.Fatalf("gemini key: got %q, %v", got, err)
	}
	if got, err := keys.Credential(context.Background(), "openrouter"); err != nil || got != "or-key" {
		t.Fatalf("openrouter key: got %q, %v", got, err)
	}
	if _, err := keys.Credential(context.Background(), "anthropic"); err == nil {
		t.Fatal("an unconfigured provider should error, not return an empty key")
	}
	if _, err := (apiKeys{}).Credential(context.Background(), "gemini"); err == nil {
		t.Fatal("an empty key set should error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := keys.Credential(ctx, "gemini"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context should win: got %v", err)
	}
}

func TestPlanProvidersAuto(t *testing.T) {
	both, err := planProviders("auto", apiKeys{"gemini": "g", "openrouter": "o"})
	if err != nil {
		t.Fatalf("auto with both keys: %v", err)
	}
	if len(both.providers) != 2 {
		t.Fatalf("auto with both keys should wire two providers, got %d", len(both.providers))
	}
	if both.providers[0].Name() != "gemini" || both.providers[1].Name() != "openrouter" {
		t.Fatalf("gemini must be registered before the permissive openrouter adapter, got %s then %s",
			both.providers[0].Name(), both.providers[1].Name())
	}
	if len(both.notices) != 0 {
		t.Fatalf("a fully configured auto plan should be silent, got %v", both.notices)
	}

	orOnly, err := planProviders("auto", apiKeys{"openrouter": "o"})
	if err != nil {
		t.Fatalf("auto with only openrouter: %v", err)
	}
	if len(orOnly.providers) != 1 || orOnly.providers[0].Name() != "openrouter" {
		t.Fatalf("auto with only an openrouter key should wire openrouter alone, got %v", names(orOnly.providers))
	}

	geminiOnly, err := planProviders("auto", apiKeys{"gemini": "g"})
	if err != nil {
		t.Fatalf("auto with only gemini: %v", err)
	}
	if len(geminiOnly.providers) != 1 || geminiOnly.providers[0].Name() != "gemini" {
		t.Fatalf("auto with only a gemini key should wire gemini alone, got %v", names(geminiOnly.providers))
	}

	none, err := planProviders("auto", apiKeys{})
	if err != nil {
		t.Fatalf("auto with no keys: %v", err)
	}
	if len(none.providers) != 1 || none.providers[0].Name() != "gemini" {
		t.Fatalf("no keys should fall back to the historic keyless gemini setup, got %v", names(none.providers))
	}
	if len(none.notices) == 0 {
		t.Fatal("no keys should warn")
	}
}

func TestPlanProvidersPinned(t *testing.T) {
	forced, err := planProviders("openrouter", apiKeys{})
	if err != nil {
		t.Fatalf("forced openrouter: %v", err)
	}
	if len(forced.providers) != 1 || forced.providers[0].Name() != "openrouter" {
		t.Fatalf("-provider openrouter should wire openrouter even keyless, got %v", names(forced.providers))
	}
	if len(forced.notices) == 0 {
		t.Fatal("a keyless forced provider should warn")
	}

	gemini, err := planProviders("gemini", apiKeys{"gemini": "g", "openrouter": "o"})
	if err != nil {
		t.Fatalf("forced gemini: %v", err)
	}
	if len(gemini.providers) != 1 || gemini.providers[0].Name() != "gemini" {
		t.Fatalf("-provider gemini should ignore the openrouter key, got %v", names(gemini.providers))
	}

	if _, err := planProviders("openroute", apiKeys{"gemini": "g"}); err == nil {
		t.Fatal("a typo'd -provider should error, not silently fall back to auto")
	}
}

// TestProviderOrderRoutesModels pins the load-bearing registration order: a
// Gemini model must go to the direct client even though OpenRouter serves the
// same model under a prefixed ID, and an OpenRouter ID must fall through to
// the permissive adapter.
func TestProviderOrderRoutesModels(t *testing.T) {
	plan, err := planProviders("auto", apiKeys{"gemini": "g", "openrouter": "o"})
	if err != nil {
		t.Fatalf("plan providers: %v", err)
	}
	claims := func(model string) string {
		for _, p := range plan.providers {
			if _, err := p.Capabilities(model); err == nil {
				return p.Name()
			}
		}
		return ""
	}
	if got := claims("gemini-2.5-pro"); got != "gemini" {
		t.Fatalf("gemini-2.5-pro should be claimed by gemini, got %q", got)
	}
	if got := claims("openai/gpt-4o-mini"); got != "openrouter" {
		t.Fatalf("openai/gpt-4o-mini should be claimed by openrouter, got %q", got)
	}
}

type modelsStub struct {
	provider.Provider
	name   string
	models []provider.ModelInfo
	err    error
}

func (s modelsStub) Name() string { return s.name }

func (s modelsStub) Models(context.Context) ([]provider.ModelInfo, error) {
	return s.models, s.err
}

func TestLoadModelsKeepsGoingWhenOneCatalogueFails(t *testing.T) {
	gemini := modelsStub{name: "gemini", models: []provider.ModelInfo{{ID: "gemini-2.5-pro"}}}
	broken := modelsStub{
		Provider: nil,
		name:     "openrouter",
		err:      errors.New("dial openrouter.ai: connection refused"),
	}

	models, notices := loadModels(context.Background(), []provider.Provider{gemini, broken})
	if len(models) == 0 {
		t.Fatal("a failed openrouter fetch should not empty the picker")
	}
	if models[0].ID != "gemini-2.5-pro" {
		t.Fatalf("the working catalogue should come through first, got %q", models[0].ID)
	}
	seeded := false
	for _, m := range models {
		if m.ID == "openai/gpt-4o-mini" {
			seeded = true
		}
	}
	if !seeded {
		t.Fatal("a failed openrouter fetch should seed well-known IDs")
	}
	if len(notices) != 1 {
		t.Fatalf("exactly one failure notice expected, got %v", notices)
	}

	orOnly, _ := loadModels(context.Background(), []provider.Provider{broken})
	if len(orOnly) == 0 {
		t.Fatal("openrouter-only with a failed fetch should still list the seed catalogue")
	}
}

func TestPreselectMovesModelToFront(t *testing.T) {
	models := []provider.ModelInfo{{ID: "gemini-2.5-pro"}, {ID: "openai/gpt-4o-mini"}}

	got := preselect(models, "openai/gpt-4o-mini")
	if len(got) != 2 || got[0].ID != "openai/gpt-4o-mini" || got[1].ID != "gemini-2.5-pro" {
		t.Fatalf("a known ID should move to the front without duplication, got %v", ids(got))
	}

	got = preselect(models, "openai/gpt-5-turbo")
	if len(got) != 3 || got[0].ID != "openai/gpt-5-turbo" {
		t.Fatalf("an unknown ID should be prepended so it is still selectable, got %v", ids(got))
	}

	got = preselect(models, "")
	if len(got) != 2 || got[0].ID != "gemini-2.5-pro" {
		t.Fatalf("an empty -model should leave the catalogue untouched, got %v", ids(got))
	}
}

func TestWindowAroundKeepsCursorVisible(t *testing.T) {
	if lo, hi := windowAround(3, 5, 12); lo != 0 || hi != 5 {
		t.Fatalf("short lists render whole: got [%d,%d)", lo, hi)
	}
	if lo, hi := windowAround(0, 100, 12); lo != 0 || hi != 12 {
		t.Fatalf("cursor at the top pins the window there: got [%d,%d)", lo, hi)
	}
	if lo, hi := windowAround(6, 100, 12); lo != 0 || hi != 12 {
		t.Fatalf("cursor near the top stays visible: got [%d,%d)", lo, hi)
	}
	if lo, hi := windowAround(50, 100, 12); lo != 44 || hi != 56 {
		t.Fatalf("a mid-list cursor sits centred: got [%d,%d)", lo, hi)
	}
	if lo, hi := windowAround(99, 100, 12); lo != 88 || hi != 100 {
		t.Fatalf("cursor at the bottom pins the window there: got [%d,%d)", lo, hi)
	}
	if lo, hi := windowAround(1, 0, 12); lo != 0 || hi != 0 {
		t.Fatalf("an empty list is an empty window: got [%d,%d)", lo, hi)
	}
}

func names(ps []provider.Provider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

func ids(ms []provider.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
