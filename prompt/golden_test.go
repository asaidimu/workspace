package prompt

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// updateGolden rewrites testdata/golden/prompt with the current output:
//
//	go test ./prompt -run TestGoldenPrompts -update
var updateGolden = flag.Bool("update", false, "rewrite the golden prompt files in testdata/golden/prompt")

// The golden documents are a stable, readable projection of everything one
// build produced: the sections in assembly order, the rendered system
// parts, the mapped messages, the request finishings, the budget and the
// diagnostics. Section bodies and system strings are deliberately both
// present: the body is what sectionBodies produced, the string is what the
// provider actually receives.

type goldenDoc struct {
	Sections        []goldenSection `json:"sections"`
	System          []string        `json:"system"`
	Messages        []goldenMessage `json:"messages"`
	Tools           []goldenTool    `json:"tools,omitempty"`
	OutputSchema    string          `json:"output_schema,omitempty"`
	Model           string          `json:"model"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Budget          goldenBudget    `json:"budget"`
	Diagnostics     []goldenDiag    `json:"diagnostics,omitempty"`
}

type goldenSection struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Order int    `json:"order"`
	Body  string `json:"body"`
}

type goldenBudget struct {
	ContextWindow  int `json:"context_window"`
	ReservedOutput int `json:"reserved_output"`
	Used           int `json:"used"`
}

type goldenMessage struct {
	Role  string       `json:"role"`
	Parts []goldenPart `json:"parts"`
}

type goldenTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Schema      string `json:"schema,omitempty"`
}

type goldenDiag struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	Ref     *goldenRef `json:"ref,omitempty"`
}

type goldenRef struct {
	Key     string `json:"key"`
	Version int    `json:"version"`
}

type goldenPart struct {
	Kind      string          `json:"kind"`
	Text      string          `json:"text,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Signature string          `json:"signature,omitempty"`
	RemoteID  string          `json:"remote_id,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Size      int64           `json:"size,omitempty"`
}

func roleName(r provider.Role) string {
	switch r {
	case provider.AssistantRole:
		return "assistant"
	case provider.ToolRole:
		return "tool"
	default:
		return "user"
	}
}

func partView(p provider.Part) goldenPart {
	switch v := p.(type) {
	case provider.TextPart:
		return goldenPart{Kind: "text", Text: v.Text}
	case provider.ThinkingPart:
		return goldenPart{Kind: "thinking", Text: v.Text, Signature: v.Signature}
	case provider.ToolCallPart:
		return goldenPart{Kind: "tool_call", CallID: v.CallID, Name: v.Name, Arguments: v.Arguments}
	case provider.ToolResultPart:
		return goldenPart{Kind: "tool_result", CallID: v.CallID, Output: v.Output, IsError: v.IsError}
	case provider.ImagePart:
		return goldenPart{Kind: "image", RemoteID: v.RemoteID, MediaType: sourceMediaType(v.Source), Size: sourceSize(v.Source)}
	case provider.FilePart:
		return goldenPart{Kind: "file", Name: v.Name, RemoteID: v.RemoteID, MediaType: sourceMediaType(v.Source), Size: sourceSize(v.Source)}
	default:
		return goldenPart{Kind: "unknown"}
	}
}

func sourceMediaType(s provider.Source) string {
	if s == nil {
		return ""
	}
	return s.MediaType()
}

func sourceSize(s provider.Source) int64 {
	if s == nil {
		return 0
	}
	return s.Size()
}

func systemStrings(parts []provider.Part) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if tp, ok := p.(provider.TextPart); ok {
			out = append(out, tp.Text)
			continue
		}
		raw, _ := json.Marshal(partView(p))
		out = append(out, string(raw))
	}
	return out
}

// renderGolden captures the finished build into the golden projection.
func renderGolden(b *Build) goldenDoc {
	doc := goldenDoc{
		Model:           b.Request.Model,
		MaxOutputTokens: b.Request.MaxOutputTokens,
		Sections:        []goldenSection{},
		System:          []string{},
		Messages:        []goldenMessage{},
		Budget: goldenBudget{
			ContextWindow:  b.Budget.ContextWindow,
			ReservedOutput: b.Budget.ReservedOutput,
			Used:           b.Budget.Used,
		},
	}
	if len(b.Request.OutputSchema) > 0 {
		doc.OutputSchema = string(b.Request.OutputSchema)
	}
	doc.System = append(doc.System, systemStrings(b.Request.System)...)
	for _, s := range b.Sections {
		doc.Sections = append(doc.Sections, goldenSection{ID: s.ID, Title: s.Title, Order: s.Order, Body: s.Body})
	}
	for _, m := range b.Request.Messages {
		gm := goldenMessage{Role: roleName(m.Role), Parts: []goldenPart{}}
		for _, p := range m.Parts {
			gm.Parts = append(gm.Parts, partView(p))
		}
		doc.Messages = append(doc.Messages, gm)
	}
	for _, t := range b.Request.Tools {
		doc.Tools = append(doc.Tools, goldenTool{Name: t.Name, Description: t.Description, Schema: string(t.Schema)})
	}
	for _, d := range b.Diagnostics {
		gd := goldenDiag{Code: d.Code, Message: d.Message}
		if d.Ref != nil {
			gd.Ref = &goldenRef{Key: d.Ref.Key, Version: d.Ref.Version}
		}
		doc.Diagnostics = append(doc.Diagnostics, gd)
	}
	return doc
}

// buildGolden runs the full default pipeline with a capture stage appended
// after map, so the golden sees exactly what a caller would.
func buildGolden(t *testing.T, in Input) goldenDoc {
	t.Helper()
	var doc goldenDoc
	capture := captureStage{name: "capture", fn: func(ctx context.Context, b *Build) error {
		doc = renderGolden(b)
		return nil
	}}
	p := NewPipeline(snapshotStage{}, selectStage{}, resolveStage{}, compactStage{}, assembleStage{}, mapStage{}, capture)
	if _, _, err := p.Build(context.Background(), in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return doc
}

func marshalGolden(t *testing.T, doc goldenDoc) string {
	t.Helper()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	return string(raw) + "\n"
}

// --- golden cases ---

func goldenMinimal() Input {
	return baseInput()
}

func goldenFullSections() Input {
	in := baseInput()
	in.Snapshot.Role = &domain.Role{
		ID:             "r1",
		Persona:        "You are concise.",
		PreferenceKeys: []store.ContentRef{{Key: "pref1", Version: 1}},
	}
	in.Snapshot.Attached = []domain.Content{
		mkPref("pref1", 1),
		mkContent("pref-1", 1, domain.ModeRead, []string{"ctx"}),
		mkContent("cfg-1", 2, domain.ModeReadWrite, []string{"cfg"}),
	}
	in.Instructions = "Cite sources."
	return in
}

func goldenExtensions() Input {
	in := baseInput()
	in.Snapshot.Role = &domain.Role{ID: "r1", Persona: "Persona."}
	in.Extensions = []Extension{
		{Label: "footer", Content: "Footer.", Position: "append"},
		{Label: "legal", Content: "Legal notice.", Position: "before:persona"},
		{Label: "company", Content: "Company policy.", Position: "prepend"},
		{Label: "postpol", Content: "Post policy.", Position: "after:policy"},
		{Label: "orphan", Content: "Orphan.", Position: "before:does-not-exist"},
	}
	return in
}

func goldenStructured() Input {
	in := baseInput()
	in.Structured = true
	in.Tools = []provider.ToolSpec{
		{Name: "search", Description: "find things", Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)},
	}
	in.OutputSchema = json.RawMessage(`{"type":"object","properties":{"blocks":{"type":"array"}},"required":["blocks"]}`)
	return in
}

func goldenNativeTools() Input {
	in := baseInput()
	in.Tools = []provider.ToolSpec{
		{Name: "search", Description: "find things", Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
	}
	return in
}

// goldenHybrid teaches the block markup from the registry alongside native
// tools: prose plus parseable fences, no JSON-only preamble.
func goldenHybrid() Input {
	in := baseInput()
	in.Content = content.NewRegistry()
	in.NativeTools = true
	in.Tools = []provider.ToolSpec{
		{Name: "search", Description: "find things", Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
	}
	return in
}

func goldenTranscriptTools() Input {
	in := baseInput()
	in.Snapshot.Transcript = []domain.Turn{
		textTurn("t1", "user", "weather?"),
		toolCallTurn("t2", "c1", "getWeather"),
		toolResultTurn("t3", "c1", "sunny"),
		{ID: "t4", Version: 1, Actor: domain.Actor{Kind: domain.ActorAgent, ID: "a"},
			Blocks: []domain.Block{{Type: "thinking", Key: "k-t4", Value: map[string]any{"thinking": "pondering"}}}},
		{ID: "t5", Version: 1, Actor: domain.Actor{Kind: domain.ActorAgent, ID: "a"},
			Blocks: []domain.Block{{Type: "json", Key: "k-t5", Value: map[string]any{"a": 1}}}},
		{ID: "t6", Version: 1, Actor: domain.Actor{Kind: domain.ActorUser, ID: "u"},
			Blocks: []domain.Block{{Type: "image", Key: "k-t6", Value: map[string]any{"url": "http://x/y.png"}}}},
	}
	return in
}

func goldenResolveDiagnostics() Input {
	in := baseInput()
	tomb, _ := json.Marshal(map[string]any{"text": "gone"})
	in.Snapshot.Role = &domain.Role{
		ID: "r1",
		PreferenceKeys: []store.ContentRef{
			{Key: "conflict1", Version: 1},
			{Key: "tomb1", Version: 1},
			{Key: "ghost1", Version: 1},
		},
	}
	in.Snapshot.Settings.GlobalPreferenceKeys = []store.ContentRef{{Key: "conflict1", Version: 1}}
	in.Snapshot.Attached = []domain.Content{
		{Key: "conflict1", Version: 1, Owner: "u1", Type: "text", Mode: domain.ModeRead, Value: mustJSON(`{"text":"shared"}`), Tombstoned: false},
		{Key: "tomb1", Version: 1, Owner: "u1", Type: "text", Mode: domain.ModeRead, Value: tomb, Tombstoned: true},
	}
	return in
}

func goldenElision() Input {
	in := baseInput()
	in.ContextWindow = 25
	in.MaxOutputTokens = 5
	in.Tokenizer = TokenizerFunc(func(model string, parts []provider.Part) (int, error) {
		return len(parts) * 10, nil
	})
	return in
}

func mustJSON(s string) json.RawMessage { return json.RawMessage(s) }

func goldenCases() map[string]Input {
	return map[string]Input{
		"minimal":             goldenMinimal(),
		"full-sections":       goldenFullSections(),
		"extensions":          goldenExtensions(),
		"structured":          goldenStructured(),
		"native-tools":        goldenNativeTools(),
		"hybrid":              goldenHybrid(),
		"transcript-tools":    goldenTranscriptTools(),
		"resolve-diagnostics": goldenResolveDiagnostics(),
		"elision":             goldenElision(),
	}
}

func TestGoldenPrompts(t *testing.T) {
	dir := filepath.Join("..", "testdata", "golden", "prompt")
	cases := goldenCases()

	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		for name, in := range cases {
			got := marshalGolden(t, buildGolden(t, in))
			if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(got), 0o644); err != nil {
				t.Fatalf("write golden %s: %v", name, err)
			}
		}
		t.Logf("updated %d golden files in %s", len(cases), dir)
		return
	}

	// Stable iteration order so a mismatch report is reproducible.
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run `go test ./prompt -run TestGoldenPrompts -update` first): %v", err)
			}
			got := marshalGolden(t, buildGolden(t, cases[name]))
			if got != string(want) {
				t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
			}
		})
	}
}

// TestGoldenPromptsAreDeterministic rebuilds every case twice; two renders
// of the same input must be byte-identical without touching the files.
func TestGoldenPromptsAreDeterministic(t *testing.T) {
	names := make([]string, 0)
	for name := range goldenCases() {
		names = append(names, name)
	}
	for _, name := range names {
		in := goldenCases()[name]
		first := marshalGolden(t, buildGolden(t, in))
		second := marshalGolden(t, buildGolden(t, in))
		if first != second {
			t.Errorf("%s is not deterministic:\n%s\n---\n%s", name, first, second)
		}
	}
	if len(names) == 0 {
		t.Fatal("no golden cases")
	}
}
