// Command record captures live HTTP exchanges from the provider adapters
// into the committed recordings under testdata/http/. It is the refresh
// path for those transcripts; CI never runs it because CI has no vendor
// credentials.
//
// Usage (from the module root):
//
//	go run ./examples/record -provider anthropic -name generate-basic
//
// The provider key is read from the environment: ANTHROPIC_API_KEY,
// OPENAI_API_KEY or GEMINI_API_KEY (GOOGLE_API_KEY also works for
// Gemini). The command refuses to run without one, and never prints or
// writes the key: captured request headers are stripped to an
// placeholder before the recording is saved.
//
// Strip-secrets step: after capture, review the JSON before committing.
// The only secret that can appear is inside response bodies (the vendors
// echo request ids, never keys); the Authorization / x-goog-api-key
// headers are replaced with "Bearer test-key" / "test-key" placeholders
// by this tool, but a manual review is still the last gate.
//
// Known limitation of this skeleton: the captured request and response
// bodies are re-encoded through the provider port, not the vendors' raw
// wire format, so the output is a reference for refreshing a committed
// recording rather than a drop-in replacement for one. Raw-transport
// capture (a recording RoundTripper behind the adapters' WithHTTP) is
// the follow-up; the committed testdata/http recordings were authored
// from the vendors' public API docs and replay through recordtest.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/anthropic"
	"github.com/asaidimu/workspace/provider/gemini"
	"github.com/asaidimu/workspace/provider/openai"
)

func main() {
	var (
		prov       = flag.String("provider", "", "vendor: anthropic, openai or gemini")
		model      = flag.String("model", "", "model id to call")
		name       = flag.String("name", "", "recording name (testdata/http/<provider>/<name>.json)")
		promptText = flag.String("prompt", "hello", "user message to send")
		stream     = flag.Bool("stream", false, "capture a streaming call instead of Generate")
		tools      = flag.Bool("tools", false, "include one sample tool so a tool-call exchange is captured")
	)
	flag.Parse()

	if *prov == "" || *model == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "record: -provider, -model and -name are required")
		flag.Usage()
		os.Exit(2)
	}

	p, key, err := adapterFor(*prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "record: %v\n", err)
		os.Exit(1)
	}
	if key == "" {
		fmt.Fprintf(os.Stderr, "record: no credentials in the environment for %s "+
			"(set ANTHROPIC_API_KEY, OPENAI_API_KEY or GEMINI_API_KEY); "+
			"without a key there is nothing to record\n", *prov)
		os.Exit(1)
	}

	req := provider.Request{
		Model:           *model,
		System:          []provider.Part{provider.TextPart{Text: "Be concise."}},
		Messages:        []provider.Message{{Role: provider.UserRole, Parts: []provider.Part{provider.TextPart{Text: *promptText}}}},
		MaxOutputTokens: 256,
	}
	if *tools {
		req.Tools = []provider.ToolSpec{{
			Name:        "getWeather",
			Description: "Get current weather for a city.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var body string
	var status = 200
	if *stream {
		body, err = captureStream(ctx, p, req)
	} else {
		body, err = captureGenerate(ctx, p, req)
	}
	if err != nil {
		// A vendor error is still a recording: save it as an error
		// transcript so the adapter's mapping test can replay it. The
		// adapters' errors carry the vendor status in Error(); the
		// skeleton records the message verbatim and lets the reviewer
		// set the status when refreshing a committed error fixture.
		status = 400
		body = string(errorBody(err))
	}

	out := map[string]any{
		"name": *name,
		"request": map[string]any{
			"method": "POST",
			"path":   pathFor(*prov, *model, *stream),
			"headers": map[string]string{
				authHeader(*prov): authPlaceholder(*prov),
			},
			"body": requestJSON(req, *stream),
		},
		"response": map[string]any{
			"status": status,
		},
	}
	resp := out["response"].(map[string]any)
	if *stream {
		resp["body_text"] = body
	} else {
		resp["body"] = json.RawMessage(body)
	}

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "record: encode: %v\n", err)
		os.Exit(1)
	}
	dir := filepath.Join("testdata", "http", *prov)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "record: %v\n", err)
		os.Exit(1)
	}
	dest := filepath.Join(dir, *name+".json")
	if err := os.WriteFile(dest, append(raw, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "record: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", dest)
	fmt.Println("review it for secrets before committing (see the package comment)")
}

func adapterFor(name string) (provider.Provider, string, error) {
	switch name {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		return anthropic.New(staticCreds(key)), key, nil
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		return openai.New(staticCreds(key)), key, nil
	case "gemini":
		key := firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY"))
		return gemini.New(staticCreds(key)), key, nil
	default:
		return nil, "", fmt.Errorf("unknown provider %q (want anthropic, openai or gemini)", name)
	}
}

func captureGenerate(ctx context.Context, p provider.Provider, req provider.Request) (string, error) {
	resp, err := p.Generate(ctx, req)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func captureStream(ctx context.Context, p provider.Provider, req provider.Request) (string, error) {
	s, err := p.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer s.Close()
	var sb strings.Builder
	for {
		ev, err := s.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return sb.String(), err
			}
			break // io.EOF or a terminal vendor error ends the capture
		}
		line, mErr := json.Marshal(ev)
		if mErr != nil {
			return "", mErr
		}
		sb.WriteString("data: ")
		sb.Write(line)
		sb.WriteString("\n\n")
	}
	return sb.String(), nil
}

func requestJSON(req provider.Request, stream bool) map[string]any {
	// The recording's request body must match what the adapter sends, so
	// this mirrors the common fields; vendor-specific extras (tools,
	// generationConfig) appear when the flags asked for them.
	body := map[string]any{
		"model": req.Model,
		"messages": []map[string]any{{
			"role":    "user",
			"content": req.Messages[0].Parts[0].(provider.TextPart).Text,
		}},
	}
	if stream {
		body["stream"] = true
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	return body
}

func pathFor(prov, model string, stream bool) string {
	switch prov {
	case "anthropic":
		return "/v1/messages"
	case "openai":
		return "/v1/chat/completions"
	default:
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		return "/v1beta/models/" + model + ":" + action
	}
}

func authHeader(prov string) string {
	if prov == "gemini" {
		return "x-goog-api-key"
	}
	return "Authorization"
}

func authPlaceholder(prov string) string {
	if prov == "gemini" {
		return "test-key"
	}
	return "Bearer test-key"
}

// errorBody re-encodes a failure in the vendors' shared error shape so
// the recording replays through the adapters' mapping tests. The
// adapters return provider/internal/httpx.Error, which cmd may not
// import, so the message is taken from Error() as-is.
func errorBody(err error) json.RawMessage {
	raw, mErr := json.Marshal(map[string]any{
		"error": map[string]any{"type": "api_error", "message": err.Error()},
	})
	if mErr != nil {
		return json.RawMessage(`{"error":{"type":"api_error","message":"record: encode failed"}}`)
	}
	return raw
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// staticCreds is a one-key CredentialSource for the capture run.
type staticCreds string

func (s staticCreds) Credential(ctx context.Context, name string) (string, error) {
	if s == "" {
		return "", errors.New("no key configured")
	}
	return string(s), nil
}
