package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/kernel"
)

type searchArgs struct {
	Query  string   `json:"query"`
	Limit  int      `json:"limit,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Nested *struct {
		Deep string `json:"deep"`
	} `json:"nested,omitempty"`
}

type searchOut struct {
	Hits int `json:"hits"`
}

func principal() kernel.Principal {
	return kernel.Principal{Owner: "u1", Subject: "u1", Kind: kernel.UserPrincipal}
}

func searchTool(t *testing.T, spec ToolSpec) Tool {
	t.Helper()
	return NewTool(spec, func(ctx context.Context, p kernel.Principal, in searchArgs) (searchOut, error) {
		return searchOut{Hits: len(in.Tags) + len(in.Query)}, nil
	})
}

func TestNewToolDerivesASchemaFromTheInputType(t *testing.T) {
	tool := searchTool(t, ToolSpec{Name: "search", Description: "find things"})
	spec := tool.Spec()
	if spec.Name != "search" || spec.Description != "find things" {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.Schema) == 0 {
		t.Fatal("a schema must be derived when the spec carries none")
	}

	var schema map[string]any
	if err := json.Unmarshal(spec.Schema, &schema); err != nil {
		t.Fatalf("schema is not an object: %v\n%s", err, spec.Schema)
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("no properties: %s", spec.Schema)
	}
	q, _ := props["query"].(map[string]any)
	if q["type"] != "string" {
		t.Fatalf("query = %v, want a string", q)
	}
	// A non-pointer, non-omitempty field is required.
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "query" {
		t.Fatalf("required = %v, want [query]", required)
	}
}

func TestNewToolIsDeterministic(t *testing.T) {
	a := searchTool(t, ToolSpec{Name: "search"}).Spec().Schema
	b := searchTool(t, ToolSpec{Name: "search"}).Spec().Schema
	if string(a) != string(b) {
		t.Fatalf("two derivations of the same type differ:\n%s\n---\n%s", a, b)
	}
}

func TestNewToolLeavesAProvidedSchemaAlone(t *testing.T) {
	given := json.RawMessage(`{"type":"object"}`)
	tool := searchTool(t, ToolSpec{Name: "search", Schema: given})
	if string(tool.Spec().Schema) != string(given) {
		t.Fatalf("a supplied schema must not be overwritten: %s", tool.Spec().Schema)
	}
}

func TestNewToolPanicsOnProgrammerMistakes(t *testing.T) {
	fn := func(ctx context.Context, p kernel.Principal, in searchArgs) (searchOut, error) {
		return searchOut{}, nil
	}
	cases := []struct {
		name string
		run  func()
	}{
		{"empty name", func() { NewTool(ToolSpec{}, fn) }},
		{"nil function", func() { NewTool[searchArgs, searchOut](ToolSpec{Name: "x"}, nil) }},
		{"non-struct input", func() {
			NewTool(ToolSpec{Name: "x"}, func(ctx context.Context, p kernel.Principal, in string) (searchOut, error) {
				return searchOut{}, nil
			})
		}},
	}
	for _, c := range cases {
		if !didPanic(c.run) {
			t.Errorf("%s must panic", c.name)
		}
	}
}

func didPanic(fn func()) (ok bool) {
	defer func() { ok = recover() != nil }()
	fn()
	return
}

func TestToolCallDecodesArgumentsAndEncodesTheResult(t *testing.T) {
	tool := searchTool(t, ToolSpec{Name: "search"})
	res, err := tool.Call(context.Background(), principal(), ToolCall{
		CallID: "c1", Name: "search",
		Arguments: json.RawMessage(`{"query":"go","tags":["a","b"]}`),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	var out searchOut
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if out.Hits != 4 { // 2 for "go" + 2 tags
		t.Fatalf("hits = %d, want 4", out.Hits)
	}
}

func TestToolCallWithBadArgumentsIsAnErrorResult(t *testing.T) {
	tool := searchTool(t, ToolSpec{Name: "search"})
	res, err := tool.Call(context.Background(), principal(), ToolCall{
		CallID: "c1", Name: "search",
		Arguments: json.RawMessage(`{"limit":"not a number"}`),
	})
	if err != nil {
		t.Fatalf("a decoding failure must not fail the step: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a decoding failure must be an error result: %s", res.Output)
	}
	if !strings.Contains(string(res.Output), "error") {
		t.Fatalf("the model should see why: %s", res.Output)
	}
}

func TestToolCallRejectsTheWrongName(t *testing.T) {
	tool := searchTool(t, ToolSpec{Name: "search"})
	_, err := tool.Call(context.Background(), principal(), ToolCall{Name: "other"})
	if !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
}

func TestToolCallTruncatesAnOversizedResult(t *testing.T) {
	tool := NewTool(ToolSpec{Name: "big", MaxOutput: 40}, func(ctx context.Context, p kernel.Principal, in searchArgs) (map[string]string, error) {
		return map[string]string{"blob": strings.Repeat("x", 500)}, nil
	})
	res, err := tool.Call(context.Background(), principal(), ToolCall{Name: "big", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var flagged map[string]any
	if err := json.Unmarshal(res.Output, &flagged); err != nil {
		t.Fatalf("a truncated result must stay valid JSON: %v\n%s", err, res.Output)
	}
	if flagged["truncated"] != true {
		t.Fatalf("a truncated result must be flagged: %s", res.Output)
	}
	if len(res.Output) > 400 {
		t.Fatalf("truncated output is %d bytes, want far under the original", len(res.Output))
	}
}

func TestToolCallKeepsAFittingResultIntact(t *testing.T) {
	tool := NewTool(ToolSpec{Name: "small", MaxOutput: 40}, func(ctx context.Context, p kernel.Principal, in searchArgs) (map[string]string, error) {
		return map[string]string{"v": "ok"}, nil
	})
	res, err := tool.Call(context.Background(), principal(), ToolCall{Name: "small", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(string(res.Output), "truncated") {
		t.Fatalf("a fitting result must not be wrapped: %s", res.Output)
	}
}

func TestDeriveSchemaCoversNestedAndCollectionShapes(t *testing.T) {
	type inner struct {
		Deep string `json:"deep"`
	}
	type shape struct {
		S     string          `json:"s"`
		N     *inner          `json:"n,omitempty"`
		Tags  []string        `json:"tags"`
		Meta  map[string]int  `json:"meta,omitempty"`
		Raw   json.RawMessage `json:"raw,omitempty"`
		Count int             `json:"count"`
		Float float64         `json:"float"`
		Flag  bool            `json:"flag"`
		Skip  string          `json:"-"`
		unexp string          //nolint:unused // deliberately unexported
	}

	raw, err := content.JSONSchemaFor(reflect.TypeFor[shape]())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	props := schema["properties"].(map[string]any)
	for _, name := range []string{"s", "n", "tags", "meta", "raw", "count", "float", "flag"} {
		if _, ok := props[name]; !ok {
			t.Errorf("property %q missing from %s", name, raw)
		}
	}
	if _, ok := props["Skip"]; ok {
		t.Error("a `json:\"-\"` field must be excluded")
	}
	if _, ok := props["unexp"]; ok {
		t.Error("an unexported field must be excluded")
	}
	if props["s"].(map[string]any)["type"] != "string" {
		t.Errorf("s = %v", props["s"])
	}
	if tags, ok := props["tags"].(map[string]any); !ok || tags["type"] != "array" {
		t.Errorf("tags = %v", props["tags"])
	}
	if meta, ok := props["meta"].(map[string]any); !ok || meta["type"] != "object" {
		t.Errorf("meta = %v", props["meta"])
	}
	// A pointer field is nullable, which widens the type to [type, null].
	n, _ := props["n"].(map[string]any)
	if _, isList := n["type"].([]any); !isList {
		t.Errorf("a pointer field must be nullable, got %v", n["type"])
	}
	// A value field is not.
	if _, isList := props["count"].(map[string]any)["type"].([]any); isList {
		t.Error("a value field must not be nullable")
	}
}

func TestToolCallWrapsAnInvalidErrorAsAResult(t *testing.T) {
	type anyArgs struct {
		Arg string `json:"arg"`
	}
	tool := NewTool(ToolSpec{Name: "picky"}, func(ctx context.Context, p kernel.Principal, in anyArgs) (map[string]string, error) {
		return nil, kernel.NewError(kernel.CodeInvalid, "that argument is not allowed")
	})
	res, err := tool.Call(context.Background(), principal(), ToolCall{Name: "picky", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("an INVALID failure is the model's to handle: %v", err)
	}
	if !res.IsError {
		t.Fatalf("output = %s, want an error result", res.Output)
	}
	if !strings.Contains(string(res.Output), "not allowed") {
		t.Errorf("the model should see why: %s", res.Output)
	}
}
