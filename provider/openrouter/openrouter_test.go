package openrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asaidimu/workspace/provider"
)

type testCreds struct {
	key  string
	name string
}

func (c *testCreds) Credential(ctx context.Context, providerName string) (string, error) {
	c.name = providerName
	return c.key, nil
}

func modelsPayload() string {
	return `{"data":[
		{
			"id":"anthropic/claude-sonnet-4",
			"context_length":200000,
			"architecture":{"input_modalities":["text","image"]},
			"supported_parameters":["tools","response_format","reasoning"],
			"top_provider":{"max_completion_tokens":64000},
			"pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003"}
		},
		{
			"id":"meta-llama/llama-4-maverick",
			"context_length":1048576,
			"architecture":{"input_modalities":["text"]},
			"supported_parameters":["tools"],
			"top_provider":{"max_completion_tokens":16384},
			"pricing":{"prompt":"0.0000002","completion":"0.0000006"}
		}
	]}`
}

func TestModelsReadsTheLiveCatalogue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload()))
	}))
	defer srv.Close()

	creds := &testCreds{key: "sk-or-v1-test"}
	c := New(creds, WithBaseURL(srv.URL))

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if got := creds.name; got != "openrouter" {
		t.Errorf("credential asked for %q, want openrouter", got)
	}

	claude := models[0]
	if claude.ID != "anthropic/claude-sonnet-4" {
		t.Errorf("model id = %q", claude.ID)
	}
	want := provider.Pricing{
		Currency:            "USD",
		InputMicrosPerMTok:  3_000_000,
		OutputMicrosPerMTok: 15_000_000,
		CachedMicrosPerMTok: 300_000,
	}
	if claude.Pricing != want {
		t.Errorf("pricing = %+v, want %+v", claude.Pricing, want)
	}
	caps := claude.Capabilities
	if !caps.Streaming || !caps.NativeTools || !caps.StructuredOutput ||
		!caps.Vision || !caps.Thinking || !caps.PromptCaching {
		t.Errorf("capabilities = %+v, want tools/structured/vision/thinking/cache", caps)
	}
	if caps.PDF {
		t.Error("text+image model should not claim PDF")
	}
	if caps.ContextWindow != 200_000 || caps.MaxOutputTokens != 64_000 {
		t.Errorf("windows = %d/%d, want 200000/64000", caps.ContextWindow, caps.MaxOutputTokens)
	}
	if caps.FileUpload {
		t.Error("OpenRouter has no Files API")
	}

	llama := models[1]
	if llama.Capabilities.Vision || llama.Capabilities.PromptCaching {
		t.Errorf("llama caps = %+v, want no vision and no cache pricing", llama.Capabilities)
	}
	if llama.Pricing.CachedMicrosPerMTok != 0 {
		t.Errorf("cached price = %d, want 0 when the gateway omits it",
			llama.Pricing.CachedMicrosPerMTok)
	}

	// The catalogue is a snapshot: a second call must not hit the network.
	srv.Close()
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("second Models: %v", err)
	}
}

func TestCapabilitiesFallBackForUnknownModels(t *testing.T) {
	c := New(&testCreds{key: "k"})

	caps, err := c.Capabilities("vendor/brand-new-model")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Streaming || !caps.NativeTools {
		t.Errorf("fallback caps = %+v, want tools and streaming", caps)
	}
	if caps.FileUpload {
		t.Error("fallback must not promise a Files API")
	}
}

func TestCapabilitiesUseTheCatalogueOnceLoaded(t *testing.T) {
	c := New(&testCreds{key: "k"}, WithModels([]provider.ModelInfo{{
		ID: "text/only",
		Capabilities: provider.Capabilities{
			Streaming:     true,
			ContextWindow: 8192,
		},
	}}))

	caps, err := c.Capabilities("text/only")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.NativeTools || caps.ContextWindow != 8192 {
		t.Errorf("caps = %+v, want the supplied catalogue entry", caps)
	}
}

func TestGenerateUsesTheOpenRouterEndpoint(t *testing.T) {
	var gotAuth, gotReferer, gotTitle, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"gen-1",
			"model":"anthropic/claude-sonnet-4",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello back"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":3}
		}`))
	}))
	defer srv.Close()

	creds := &testCreds{key: "sk-or-v1-test"}
	c := New(creds,
		WithBaseURL(srv.URL),
		WithAttribution("https://example.test", "workspace demo"))

	resp, err := c.Generate(context.Background(), provider.Request{
		Model: "anthropic/claude-sonnet-4",
		Messages: []provider.Message{{
			Role:  provider.UserRole,
			Parts: []provider.Part{provider.TextPart{Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotAuth != "Bearer sk-or-v1-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotReferer != "https://example.test" || gotTitle != "workspace demo" {
		t.Errorf("attribution = %q / %q", gotReferer, gotTitle)
	}
	if gotModel != "anthropic/claude-sonnet-4" {
		t.Errorf("model = %q", gotModel)
	}
	if creds.name != "openrouter" {
		t.Errorf("credential asked for %q, want openrouter", creds.name)
	}
	text := resp.Message.Parts[0].(provider.TextPart).Text
	if text != "hello back" {
		t.Errorf("text = %q", text)
	}
	if resp.Usage.InputTokens != 4 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestModelsReportsGatewayErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(&testCreds{key: "k"}, WithBaseURL(srv.URL))
	_, err := c.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "openrouter:") {
		t.Fatalf("Models error = %v, want an openrouter-prefixed error", err)
	}
}

func TestUploadFileIsUnsupported(t *testing.T) {
	c := New(&testCreds{key: "k"})
	_, err := c.UploadFile(context.Background(), nil, "x.png")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("UploadFile error = %v, want unsupported", err)
	}
}
