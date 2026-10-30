package content

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/asaidimu/workspace/provider"
)

func TestParseTextProseOnly(t *testing.T) {
	r := NewRegistry()
	text := "Hello world!\nThis is ordinary prose.\nAnother line."
	blocks, diags, err := r.ParseText(text)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Type != "text" {
		t.Errorf("block type = %s, want text", blocks[0].Type)
	}
	p, ok := blocks[0].Value.(textPayload)
	if !ok || p.Text != text {
		t.Errorf("block value = %+v, want %q", blocks[0].Value, text)
	}
}

func TestParseTextMultipleFences(t *testing.T) {
	r := NewRegistry()
	input := `Introductory prose.

` + "```workspace:json" + `
{"value": {"rows": 3, "ok": true}}
` + "```" + `

Middle prose between blocks.

` + "```workspace:document" + `
{"title": "Summary", "text": "A brief doc"}
` + "```" + `

Trailing prose.`

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if len(blocks) != 5 {
		t.Fatalf("expected 5 blocks, got %d", len(blocks))
	}

	// 0: text
	if blocks[0].Type != "text" {
		t.Errorf("block 0 type = %s, want text", blocks[0].Type)
	}
	// 1: json
	if blocks[1].Type != "json" {
		t.Errorf("block 1 type = %s, want json", blocks[1].Type)
	}
	jsonVal, ok := blocks[1].Value.(map[string]any)
	if !ok {
		t.Fatalf("block 1 value is not map[string]any: %T", blocks[1].Value)
	}
	if jsonVal["value"] == nil {
		t.Errorf("block 1 jsonVal[value] missing: %+v", jsonVal)
	}
	// 2: text
	if blocks[2].Type != "text" {
		t.Errorf("block 2 type = %s, want text", blocks[2].Type)
	}
	// 3: document
	if blocks[3].Type != "document" {
		t.Errorf("block 3 type = %s, want document", blocks[3].Type)
	}
	docVal, ok := blocks[3].Value.(documentPayload)
	if !ok || docVal.Title != "Summary" || docVal.Text == nil || *docVal.Text != "A brief doc" {
		t.Errorf("block 3 docVal unexpected: %+v", blocks[3].Value)
	}
	// 4: text
	if blocks[4].Type != "text" {
		t.Errorf("block 4 type = %s, want text", blocks[4].Type)
	}
}

func TestParseTextAdjacentFences(t *testing.T) {
	r := NewRegistry()
	input := "```workspace:json\n{\"a\": 1}\n```\n```workspace:document\n{\"title\": \"Doc\"}\n```"

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Type != "json" || blocks[1].Type != "document" {
		t.Errorf("types = %s, %s, want json, document", blocks[0].Type, blocks[1].Type)
	}
}

func TestParseTextUnclosedFence(t *testing.T) {
	r := NewRegistry()
	input := "Before prose\n```workspace:json\n{\"unclosed\": true}"

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(diags) != 1 || diags[0].Code != "unterminated" {
		t.Fatalf("expected 1 unterminated diag, got %+v", diags)
	}
	// The unclosed fence remains as trailing prose.
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (before prose + unclosed fence prose), got %d", len(blocks))
	}
	p0 := blocks[0].Value.(textPayload)
	if p0.Text != "Before prose" {
		t.Errorf("p0 text = %q, want Before prose", p0.Text)
	}
	p1 := blocks[1].Value.(textPayload)
	wantP1 := "```workspace:json\n{\"unclosed\": true}"
	if p1.Text != wantP1 {
		t.Errorf("p1 text = %q, want %q", p1.Text, wantP1)
	}
}

func TestParseTextInvalidJSON(t *testing.T) {
	r := NewRegistry()
	input := "```workspace:json\n{invalid json\n```"

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "unknown" {
		t.Fatalf("expected 1 unknown block, got %+v", blocks)
	}
	if len(diags) != 1 || diags[0].Code != "invalid" {
		t.Fatalf("expected 1 invalid diagnostic, got %+v", diags)
	}
	// Original bytes survive in raw
	if string(blocks[0].Raw) != "{invalid json" {
		t.Errorf("blocks[0].Raw = %q, want {invalid json", string(blocks[0].Raw))
	}
}

func TestParseTextUnknownType(t *testing.T) {
	r := NewRegistry()
	input := "```workspace:nonexistent\n{\"a\": 1}\n```"

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "unknown" {
		t.Fatalf("expected 1 unknown block, got %+v", blocks)
	}
	if len(diags) != 1 || diags[0].Code != "unknown_type" {
		t.Fatalf("expected 1 unknown_type diag, got %+v", diags)
	}
	val, ok := blocks[0].Value.(map[string]any)
	if !ok || val["originalType"] != "nonexistent" {
		t.Errorf("originalType = %v, want nonexistent", val["originalType"])
	}
}

func TestParseTextNonEmittableType(t *testing.T) {
	r := NewRegistry()
	// image is Consumable but NOT Emittable
	input := "```workspace:image\n{\"ref\": \"blob-123\"}\n```"

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "unknown" {
		t.Fatalf("expected 1 unknown block, got %+v", blocks)
	}
	if len(diags) != 1 || diags[0].Code != "non_emittable" {
		t.Fatalf("expected 1 non_emittable diag, got %+v", diags)
	}
}

func TestParseTextEnvelopeForm(t *testing.T) {
	r := NewRegistry()
	input := "```workspace\n{\"type\": \"json\", \"value\": {\"rows\": 3}}\n```"

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if len(blocks) != 1 || blocks[0].Type != "json" {
		t.Fatalf("expected 1 json block, got %+v", blocks)
	}
	val, ok := blocks[0].Value.(map[string]any)
	if !ok || val["rows"] != json.Number("3") && val["rows"] != float64(3) {
		t.Errorf("val unexpected: %+v", val)
	}
}

func TestParseTextLiteralFencePreserved(t *testing.T) {
	r := NewRegistry()
	// A fence with info string "python" or "go" is ordinary markdown code block, preserved as prose.
	input := "Here is some code:\n```go\nfmt.Println(\"hi\")\n```\nDone."

	blocks, diags, err := r.ParseText(input)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("expected 1 text block, got %+v", blocks)
	}
	p := blocks[0].Value.(textPayload)
	if p.Text != input {
		t.Errorf("got %q, want %q", p.Text, input)
	}
}

func TestParsePartsNativeToolsFallback(t *testing.T) {
	r := NewRegistry()
	fenceInput := "```workspace:tool_call\n{\"call_id\": \"c1\", \"name\": \"calc\", \"arguments\": {}}\n```"

	// 1. With NativeTools = true: tool_call fence becomes unknown with native_tool_expected diag.
	partsNative := []provider.Part{provider.TextPart{Text: fenceInput}}
	blocks, diags, err := ParseParts(r, partsNative, ParseOptions{NativeTools: true})
	if err != nil {
		t.Fatalf("ParseParts failed: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "unknown" {
		t.Errorf("expected unknown block, got %+v", blocks)
	}
	if len(diags) != 1 || diags[0].Code != "native_tool_expected" {
		t.Errorf("expected native_tool_expected diag, got %+v", diags)
	}

	// 2. With NativeTools = false: tool_call fence is accepted as a tool_call block.
	blocksNonNative, diagsNonNative, err := ParseParts(r, partsNative, ParseOptions{NativeTools: false})
	if err != nil {
		t.Fatalf("ParseParts failed: %v", err)
	}
	if len(diagsNonNative) != 0 {
		t.Errorf("unexpected diags: %+v", diagsNonNative)
	}
	if len(blocksNonNative) != 1 || blocksNonNative[0].Type != "tool_call" {
		t.Errorf("expected tool_call block, got %+v", blocksNonNative)
	}
}

func TestParsePartsThinkingAndNativeToolCall(t *testing.T) {
	r := NewRegistry()
	parts := []provider.Part{
		provider.ThinkingPart{Text: "reasoning trace...", Signature: "sig-123"},
		provider.TextPart{Text: "I will now check the weather."},
		provider.ToolCallPart{
			CallID:    "call_abc",
			Name:      "getWeather",
			Arguments: json.RawMessage(`{"city": "Nairobi"}`),
		},
	}

	blocks, diags, err := ParseParts(r, parts, ParseOptions{NativeTools: true})
	if err != nil {
		t.Fatalf("ParseParts failed: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}

	if blocks[0].Type != "thinking" {
		t.Errorf("block 0 = %s, want thinking", blocks[0].Type)
	}
	th := blocks[0].Value.(thinkingPayload)
	if th.Text != "reasoning trace..." || th.Signature != "sig-123" {
		t.Errorf("block 0 thinking unexpected: %+v", th)
	}

	if blocks[1].Type != "text" {
		t.Errorf("block 1 = %s, want text", blocks[1].Type)
	}
	txt := blocks[1].Value.(textPayload)
	if txt.Text != "I will now check the weather." {
		t.Errorf("block 1 text unexpected: %+v", txt)
	}

	if blocks[2].Type != "tool_call" {
		t.Errorf("block 2 = %s, want tool_call", blocks[2].Type)
	}
	tc := blocks[2].Value.(toolCallPayload)
	if tc.CallID != "call_abc" || tc.Name != "getWeather" {
		t.Errorf("block 2 tool_call unexpected: %+v", tc)
	}
}

func TestBlockTypeRules(t *testing.T) {
	r := NewRegistry()
	rules := []string{"Field x is required", "Cannot exceed 100"}
	bt, err := NewBlockType[textPayload]("custom", Consumable|Emittable,
		WithDescription("custom type"),
		WithRules(rules),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bt.Rules(), rules) {
		t.Errorf("Rules() = %v, want %v", bt.Rules(), rules)
	}
	if err := r.Register(bt); err != nil {
		t.Fatal(err)
	}
	lookup, ok := r.Lookup("custom")
	if !ok {
		t.Fatal("lookup custom failed")
	}
	if !reflect.DeepEqual(lookup.Rules(), rules) {
		t.Errorf("lookup.Rules() = %v, want %v", lookup.Rules(), rules)
	}
}
