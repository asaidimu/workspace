// Package openrouter adapts OpenRouter's OpenAI-compatible API. It reuses the
// openai wire code and adds what the gateway needs: a different base URL and
// provider name, attribution headers, a live model catalogue with real prices,
// and a permissive fallback so model IDs the gateway adds tomorrow work
// without a new release.
package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/internal/httpx"
	"github.com/asaidimu/workspace/provider/openai"
)

// DefaultBaseURL is the gateway's OpenAI-compatible root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// Client is the OpenRouter adapter. The OpenAI-compatible methods are
// promoted from the embedded client; Models, Capabilities and UploadFile are
// OpenRouter's own.
type Client struct {
	*openai.Client

	base    string
	creds   provider.CredentialSource
	http    *httpx.Client
	headers map[string]string

	mu     sync.Mutex
	models []provider.ModelInfo
	byID   map[string]provider.ModelInfo
}

// compile-time assertion that the adapter satisfies the port.
var _ provider.Provider = (*Client)(nil)

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the adapter at another host (tests, a proxy).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.base = strings.TrimSuffix(u, "/") }
}

// WithAttribution sets OpenRouter's optional HTTP-Referer and X-Title headers.
// Either value may be empty; only non-empty values are sent.
func WithAttribution(referer, title string) Option {
	return func(c *Client) {
		if referer != "" {
			c.headers["HTTP-Referer"] = referer
		}
		if title != "" {
			c.headers["X-Title"] = title
		}
	}
}

// WithModels supplies the catalogue instead of fetching /models, for offline
// use or tests.
func WithModels(models []provider.ModelInfo) Option {
	return func(c *Client) { c.storeModels(models) }
}

// WithHTTPClient replaces the net/http client used for the catalogue call and
// the OpenAI-compatible requests.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http.HTTP = h }
}

// New returns an OpenRouter adapter. The API key is read from creds on every
// call, under the provider name "openrouter".
func New(creds provider.CredentialSource, opts ...Option) *Client {
	c := &Client{
		base:    DefaultBaseURL,
		creds:   creds,
		http:    httpx.New(),
		headers: map[string]string{},
	}
	for _, o := range opts {
		o(c)
	}
	c.Client = openai.New(creds,
		openai.WithBaseURL(c.base),
		openai.WithName("openrouter"),
		openai.WithHTTP(c.http),
		openai.WithHeaders(c.headers),
		openai.WithCapabilities(c.Capabilities),
	)
	return c
}

// Models implements provider.Provider with OpenRouter's live catalogue: real
// context windows, modalities and prices. The first call fetches and caches;
// later calls reuse the snapshot.
func (c *Client) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	c.mu.Lock()
	cached := c.models
	c.mu.Unlock()
	if cached != nil {
		return append([]provider.ModelInfo(nil), cached...), nil
	}

	fetched, err := c.fetchModels(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.models == nil {
		c.storeModels(fetched)
	}
	out := append([]provider.ModelInfo(nil), c.models...)
	c.mu.Unlock()
	return out, nil
}

// Capabilities implements provider.Provider. Known models report what the
// catalogue says; anything else gets the permissive defaults, so a model that
// appeared after the last fetch still runs.
func (c *Client) Capabilities(model string) (provider.Capabilities, error) {
	c.mu.Lock()
	m, ok := c.byID[model]
	c.mu.Unlock()
	if ok {
		return m.Capabilities, nil
	}
	return defaultCapabilities(), nil
}

// UploadFile implements provider.Provider. OpenRouter has no Files API; send
// files inline (the OpenAI-compatible wire inlines image sources) or by URL.
func (c *Client) UploadFile(ctx context.Context, src provider.Source, name string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, fmt.Errorf(
		"openrouter: file upload is not supported; send the file inline or by URL")
}

// storeModels replaces the catalogue; callers hold c.mu, except the option
// which runs before the client is shared.
func (c *Client) storeModels(models []provider.ModelInfo) {
	c.models = append([]provider.ModelInfo(nil), models...)
	c.byID = make(map[string]provider.ModelInfo, len(models))
	for _, m := range models {
		c.byID[m.ID] = m
	}
}

func (c *Client) fetchModels(ctx context.Context) ([]provider.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter: listing models: %w", err)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if key, err := c.creds.Credential(ctx, "openrouter"); err == nil && key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.http.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter: listing models: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("openrouter: reading the model list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter: listing models: http status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("openrouter: decode model list: %w", err)
	}
	out := make([]provider.ModelInfo, 0, len(payload.Data))
	for _, e := range payload.Data {
		if e.ID == "" {
			continue
		}
		out = append(out, e.info())
	}
	return out, nil
}

type modelEntry struct {
	ID                  string   `json:"id"`
	ContextLength       int      `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	Architecture        struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Pricing struct {
		Prompt         string `json:"prompt"`
		Completion     string `json:"completion"`
		InputCacheRead string `json:"input_cache_read"`
	} `json:"pricing"`
}

func (e modelEntry) info() provider.ModelInfo {
	return provider.ModelInfo{
		ID: e.ID,
		Capabilities: provider.Capabilities{
			Streaming:        true,
			NativeTools:      has(e.SupportedParameters, "tools"),
			StructuredOutput: has(e.SupportedParameters, "response_format") || has(e.SupportedParameters, "structured_outputs"),
			Vision:           has(e.Architecture.InputModalities, "image"),
			PDF:              has(e.Architecture.InputModalities, "file"),
			Thinking:         has(e.SupportedParameters, "reasoning") || has(e.SupportedParameters, "include_reasoning"),
			PromptCaching:    microsPerMTok(e.Pricing.InputCacheRead) > 0,
			ContextWindow:    e.ContextLength,
			MaxOutputTokens:  e.TopProvider.MaxCompletionTokens,
		},
		Pricing: provider.Pricing{
			Currency:            "USD",
			InputMicrosPerMTok:  microsPerMTok(e.Pricing.Prompt),
			OutputMicrosPerMTok: microsPerMTok(e.Pricing.Completion),
			CachedMicrosPerMTok: microsPerMTok(e.Pricing.InputCacheRead),
		},
	}
}

// defaultCapabilities is the permissive set used for model IDs the catalogue
// does not know. Tools, structured output and vision are the gateway's norm;
// if a specific model lacks one, OpenRouter's own 400 explains why.
func defaultCapabilities() provider.Capabilities {
	return provider.Capabilities{
		Streaming:        true,
		NativeTools:      true,
		StructuredOutput: true,
		Vision:           true,
		PDF:              true,
		Thinking:         true,
		PromptCaching:    true,
	}
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// microsPerMTok converts OpenRouter's USD-per-token string to micro-USD per
// million tokens. "0", "-1" (dynamic pricing) and unparseable values map to 0.
func microsPerMTok(s string) int64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0
	}
	return int64(math.Round(f * 1e12))
}
