package content

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
)

// updateGolden rewrites testdata/golden/registry with the current output:
//
//	go test ./content -run TestGoldenRegistry -update
var updateGolden = flag.Bool("update", false, "rewrite the golden registry file in testdata/golden/registry")

// specCaps is the section 8 built-in table, checked against the
// implementations so the docs and the code cannot drift.
var specCaps = []struct {
	name string
	caps Capability
}{
	{"text", Consumable | Emittable},
	{"thinking", Emittable},
	{"image", Consumable},
	{"document", Consumable | Emittable},
	{"file", Consumable},
	{"json", Consumable | Emittable},
	{"preference", Consumable},
	{"tool_call", Emittable},
	{"tool_result", Consumable},
	{"summary", Consumable},
	{"tool_digest", Consumable},
	{"unknown", 0},
}

func TestBuiltinsMatchSpecTable(t *testing.T) {
	r := NewRegistry()
	for _, want := range specCaps {
		bt, ok := r.Lookup(want.name)
		if !ok {
			t.Fatalf("builtin %q is missing", want.name)
		}
		if got := bt.Capabilities(); got != want.caps {
			t.Errorf("builtin %q capabilities = %v, want %v", want.name, got, want.caps)
		}
		if bt.Schema() == nil {
			t.Errorf("builtin %q has no schema", want.name)
		}
		if bt.Describe() == "" {
			t.Errorf("builtin %q has no description", want.name)
		}
	}
}

func TestRegisterDuplicate(t *testing.T) {
	r := newEmptyRegistry()
	bt, err := NewBlockType[textPayload]("text", Consumable|Emittable)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(bt); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(bt); err == nil {
		t.Fatal("duplicate register succeeded, want error")
	}
}

func TestRegisterNil(t *testing.T) {
	r := newEmptyRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("nil register succeeded, want error")
	}
}

func TestTypesSorted(t *testing.T) {
	r := NewRegistry()
	types := r.Types()
	if len(types) != len(specCaps) {
		t.Fatalf("Types() has %d entries, want %d", len(types), len(specCaps))
	}
	for i := 1; i < len(types); i++ {
		if types[i-1].Name() >= types[i].Name() {
			t.Fatalf("Types() is not sorted: %q before %q", types[i-1].Name(), types[i].Name())
		}
	}
}

func TestDecodeValidText(t *testing.T) {
	r := NewRegistry()
	bt, _ := r.Lookup("text")
	value, issues := bt.Decode(map[string]any{"text": "hello"})
	if value == nil {
		t.Fatalf("Decode returned nil value, issues: %v", issues)
	}
	p, ok := value.(textPayload)
	if !ok {
		t.Fatalf("Decode bound %T, want textPayload", value)
	}
	if p.Text != "hello" {
		t.Errorf("text = %q, want %q", p.Text, "hello")
	}
}

func TestDecodeInvalidText(t *testing.T) {
	r := NewRegistry()
	bt, _ := r.Lookup("text")
	value, issues := bt.Decode(map[string]any{"text": 42})
	if value != nil {
		t.Fatalf("Decode returned a value for a wrong-typed field: %v", value)
	}
	if len(issues) == 0 {
		t.Fatal("Decode returned no issues for a wrong-typed field")
	}
}

func TestDecodeRejectsUnexpectedField(t *testing.T) {
	r := NewRegistry()
	bt, _ := r.Lookup("text")
	value, issues := bt.Decode(map[string]any{"text": "hi", "extra": true})
	if value != nil {
		t.Fatalf("strict Decode accepted an unexpected field: %v", value)
	}
	if len(issues) == 0 {
		t.Fatal("Decode returned no issues for an unexpected field")
	}
}

func TestRoundTrip(t *testing.T) {
	r := NewRegistry()
	bt, _ := r.Lookup("tool_call")
	in := map[string]any{"call_id": "c1", "name": "getWeather", "arguments": map[string]any{"city": "Nairobi"}}
	value, issues := bt.Decode(in)
	if value == nil {
		t.Fatalf("Decode failed: %v", issues)
	}
	out, err := bt.Encode(value)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if out["call_id"] != "c1" || out["name"] != "getWeather" {
		t.Errorf("Encode round-trip lost fields: %v", out)
	}
}

func TestRenderDefaults(t *testing.T) {
	cases := []struct {
		typeName string
		value    any
		want     string
	}{
		{"text", textPayload{Text: "hello"}, "hello"},
		{"thinking", thinkingPayload{Text: "hmm"}, "hmm"},
		{"preference", preferencePayload{Name: "style", Text: "be kind"}, "style: be kind"},
		{"tool_call", toolCallPayload{CallID: "c1", Name: "getWeather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)},
			"[c1] Tool Call: getWeather({\"city\":\"Nairobi\"})"},
		{"tool_result", toolResultPayload{CallID: "c1", Output: json.RawMessage(`{"temp":25}`)}, "[c1] Tool Result: {\"temp\":25}"},
		{"tool_result", toolResultPayload{CallID: "c2", Output: json.RawMessage(`"not found"`), IsError: true}, "[c2] Tool Error: \"not found\""},
	}
	r := NewRegistry()
	for _, c := range cases {
		bt, ok := r.Lookup(c.typeName)
		if !ok {
			t.Fatalf("type %q missing", c.typeName)
		}
		got, err := bt.Render(c.value)
		if err != nil {
			t.Errorf("Render %s: %v", c.typeName, err)
			continue
		}
		if got != c.want {
			t.Errorf("Render %s = %q, want %q", c.typeName, got, c.want)
		}
	}
}

func TestMapperForWildcard(t *testing.T) {
	r := NewRegistry()
	m, ok := r.MapperFor("text", "any-provider")
	if !ok {
		t.Fatal("wildcard mapper not found for text")
	}
	parts, err := m.ToParts(t.Context(), domain.Block{Type: "text", Value: map[string]any{"text": "hi"}}, provider.Capabilities{}, MapEnv{})
	if err != nil {
		t.Fatalf("ToParts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("ToParts returned %d parts, want 1", len(parts))
	}
	tp, ok := parts[0].(provider.TextPart)
	if !ok || tp.Text != "hi" {
		t.Fatalf("part = %#v, want TextPart{hi}", parts[0])
	}
}

func TestMapperForOverridesWildcard(t *testing.T) {
	r := newEmptyRegistry()
	bt, err := NewBlockType[textPayload]("text", Consumable|Emittable)
	if err != nil {
		t.Fatal(err)
	}
	custom := mapperFunc(func(_ context.Context, _ domain.Block, _ provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
		return []provider.Part{provider.TextPart{Text: "custom"}}, nil
	})
	defaultM := mapperFunc(func(_ context.Context, _ domain.Block, _ provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
		return []provider.Part{provider.TextPart{Text: "default"}}, nil
	})
	if err := r.Register(bt, WithMapper(wildcardMapper, defaultM), WithMapper("special", custom)); err != nil {
		t.Fatal(err)
	}
	m, ok := r.MapperFor("text", "special")
	if !ok {
		t.Fatal("provider-specific mapper not found")
	}
	parts, err := m.ToParts(t.Context(), domain.Block{Type: "text"}, provider.Capabilities{}, MapEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if parts[0].(provider.TextPart).Text != "custom" {
		t.Fatalf("provider-specific mapper did not win: %v", parts[0])
	}
	m, _ = r.MapperFor("text", "other")
	parts, _ = m.ToParts(t.Context(), domain.Block{Type: "text"}, provider.Capabilities{}, MapEnv{})
	if parts[0].(provider.TextPart).Text != "default" {
		t.Fatalf("wildcard mapper not used: %v", parts[0])
	}
}

func TestUnknownHasNoMapper(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.MapperFor("unknown", "openai"); ok {
		t.Fatal("unknown type has a mapper; it must never be sent to a provider")
	}
}

func TestParseKeepsUnknownVerbatim(t *testing.T) {
	r := NewRegistry()
	raw := json.RawMessage(`[
		{"Type": "text", "Key": "k1", "Value": {"text": "hello"}},
		{"Type": "acme.chart", "Key": "k2", "Value": {"series": [1, 2, 3]}}
	]`)
	blocks, diags, err := r.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("Parse returned %d blocks, want 2", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Key != "k1" {
		t.Errorf("block 0 = %s/%s, want text/k1", blocks[0].Type, blocks[0].Key)
	}
	if p, ok := blocks[0].Value.(textPayload); !ok || p.Text != "hello" {
		t.Errorf("block 0 value = %#v, want textPayload{hello}", blocks[0].Value)
	}
	if blocks[1].Type != "unknown" {
		t.Errorf("block 1 type = %q, want unknown", blocks[1].Type)
	}
	if len(diags) != 1 || diags[0].Code != "unknown_type" || diags[0].BlockIndex != 1 {
		t.Errorf("diags = %+v, want one unknown_type at index 1", diags)
	}
	// The original bytes survive verbatim.
	var original map[string]any
	if err := json.Unmarshal([]byte(`{"Type": "acme.chart", "Key": "k2", "Value": {"series": [1, 2, 3]}}`), &original); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(blocks[1].Raw, &got); err != nil {
		t.Fatalf("block 1 Raw is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Errorf("block 1 Raw = %v, want %v", got, original)
	}
	value := blocks[1].Value.(map[string]any)
	if value["originalType"] != "acme.chart" {
		t.Errorf("originalType = %v, want acme.chart", value["originalType"])
	}
	rawMap := value["raw"].(map[string]any)
	if rawMap["Type"] != "acme.chart" {
		t.Errorf("value.raw lost the type: %v", rawMap)
	}
}

func TestParseInvalidBecomesUnknown(t *testing.T) {
	r := NewRegistry()
	raw := json.RawMessage(`[
		{"Type": "text", "Key": "k1", "Value": {"text": 42}}
	]`)
	blocks, diags, err := r.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if blocks[0].Type != "unknown" {
		t.Errorf("invalid block type = %q, want unknown", blocks[0].Type)
	}
	if len(diags) != 1 || diags[0].Code != "invalid" || len(diags[0].Issues) == 0 {
		t.Errorf("diags = %+v, want one invalid with issues", diags)
	}
}

func TestParseLowercaseKeys(t *testing.T) {
	// The wire shape of domain.Block uses Go field names, but JSON field
	// matching is case-insensitive, so lowercase also parses.
	r := NewRegistry()
	raw := json.RawMessage(`[{"type": "text", "key": "k", "value": {"text": "hi"}}]`)
	blocks, diags, err := r.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 || blocks[0].Type != "text" {
		t.Fatalf("lowercase parse failed: blocks=%+v diags=%+v", blocks, diags)
	}
}

func TestParseRejectsNonArray(t *testing.T) {
	r := NewRegistry()
	if _, _, err := r.Parse(json.RawMessage(`{"Type": "text"}`)); err == nil {
		t.Fatal("Parse accepted a non-array")
	}
}

// TestParseRoundTripThroughBlocks parses the JSON that domain.Block
// itself marshals to, which is what the store round-trips.
func TestParseRoundTripThroughBlocks(t *testing.T) {
	r := NewRegistry()
	in := []domain.Block{
		{Type: "text", Key: "k1", Value: map[string]any{"text": "hello"}},
		{Type: "json", Key: "k2", Value: map[string]any{"a": 1}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	blocks, diags, err := r.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if blocks[0].Type != "text" || blocks[1].Type != "json" {
		t.Errorf("round-trip types = %s, %s", blocks[0].Type, blocks[1].Type)
	}
}

func TestThinkingMapperDropsWithoutCapability(t *testing.T) {
	r := NewRegistry()
	m, _ := r.MapperFor("thinking", "openai")
	parts, err := m.ToParts(t.Context(), domain.Block{Type: "thinking", Value: map[string]any{"text": "hmm"}}, provider.Capabilities{}, MapEnv{})
	if err != nil {
		t.Fatalf("ToParts: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("thinking leaked into parts without the capability: %v", parts)
	}
}

func TestImageMapperDowngrades(t *testing.T) {
	r := NewRegistry()
	m, _ := r.MapperFor("image", "openai")
	parts, err := m.ToParts(t.Context(), domain.Block{Type: "image", Value: map[string]any{"ref": "blob-1"}}, provider.Capabilities{}, MapEnv{})
	if err != nil {
		t.Fatalf("ToParts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("downgrade produced %d parts, want 1", len(parts))
	}
	if tp, ok := parts[0].(provider.TextPart); !ok || tp.Text != "[image omitted]" {
		t.Fatalf("part = %#v, want the [image omitted] placeholder", parts[0])
	}
}

func TestImageMapperResolvesThroughTheEnv(t *testing.T) {
	r := NewRegistry()
	m, _ := r.MapperFor("image", "openai")
	resolved := &fakeResolver{}
	env := MapEnv{Blobs: resolved, Owner: "u1"}
	parts, err := m.ToParts(t.Context(), domain.Block{Type: "image", Value: map[string]any{"ref": "blob-1"}}, provider.Capabilities{}, env)
	if err != nil {
		t.Fatalf("ToParts: %v", err)
	}
	ip, ok := parts[0].(provider.ImagePart)
	if !ok || ip.RemoteID != "remote-1" {
		t.Fatalf("part = %#v, want the resolver's ImagePart", parts[0])
	}
	if !resolved.sawOwner || !resolved.sawRef {
		t.Fatalf("the resolver was not called with (owner=%v, ref=%v)", resolved.sawOwner, resolved.sawRef)
	}

	broken := &fakeResolver{err: errors.New("gone")}
	_, err = m.ToParts(t.Context(), domain.Block{Type: "image", Value: map[string]any{"ref": "blob-1"}}, provider.Capabilities{}, MapEnv{Blobs: broken})
	if err == nil {
		t.Fatal("a failed resolution must be an error, not a silent placeholder")
	}
}

// fakeResolver is a BlobResolver double: an ImagePart with a remote id, plus
// the calls it observed.
type fakeResolver struct {
	err              error
	sawOwner, sawRef bool
}

func (f *fakeResolver) Resolve(_ context.Context, owner, ref, providerName string) (provider.Part, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.sawOwner = owner == "u1"
	f.sawRef = ref == "blob-1"
	_ = providerName
	return provider.ImagePart{RemoteID: "remote-1"}, nil
}

func TestToolCallAndResultParts(t *testing.T) {
	r := NewRegistry()
	m, _ := r.MapperFor("tool_call", "anthropic")
	parts, err := m.ToParts(t.Context(), domain.Block{Type: "tool_call", Value: map[string]any{
		"call_id": "c1", "name": "getWeather", "arguments": json.RawMessage(`{"city":"Nairobi"}`),
	}}, provider.Capabilities{NativeTools: true}, MapEnv{})
	if err != nil {
		t.Fatalf("ToParts: %v", err)
	}
	call, ok := parts[0].(provider.ToolCallPart)
	if !ok || call.CallID != "c1" || call.Name != "getWeather" {
		t.Fatalf("part = %#v, want ToolCallPart", parts[0])
	}

	m, _ = r.MapperFor("tool_result", "anthropic")
	parts, err = m.ToParts(t.Context(), domain.Block{Type: "tool_result", Value: map[string]any{
		"call_id": "c1", "output": json.RawMessage(`{"temp":25}`), "is_error": false,
	}}, provider.Capabilities{NativeTools: true}, MapEnv{})
	if err != nil {
		t.Fatalf("ToParts: %v", err)
	}
	res, ok := parts[0].(provider.ToolResultPart)
	if !ok || res.CallID != "c1" || string(res.Output) != `{"temp":25}` {
		t.Fatalf("part = %#v, want ToolResultPart", parts[0])
	}
}

// TestGoldenRegistry pins Parse output, the capability table and render
// output. Regenerate with:
//
//	go test ./content -run TestGoldenRegistry -update
func TestGoldenRegistry(t *testing.T) {
	r := NewRegistry()

	parseRaw := json.RawMessage(`[
		{"Type": "text", "Key": "k1", "Value": {"text": "hello"}},
		{"Type": "json", "Key": "k2", "Value": {"a": 1}},
		{"Type": "tool_call", "Key": "k3", "Value": {"call_id": "c1", "name": "getWeather", "arguments": {"city": "Nairobi"}}},
		{"Type": "tool_result", "Key": "k4", "Value": {"call_id": "c1", "output": {"temp": 25}, "is_error": false}},
		{"Type": "acme.chart", "Key": "k5", "Value": {"series": [1, 2, 3]}},
		{"Type": "text", "Key": "k6", "Value": {"text": 42}}
	]`)
	blocks, diags, err := r.Parse(parseRaw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	type blockView struct {
		Type  string          `json:"type"`
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value,omitempty"`
		Raw   json.RawMessage `json:"raw,omitempty"`
	}
	views := make([]blockView, 0, len(blocks))
	for _, b := range blocks {
		v := blockView{Type: b.Type, Key: b.Key, Raw: b.Raw}
		if b.Value != nil {
			// The typed payloads are structs; render them as their JSON.
			raw, mErr := json.Marshal(b.Value)
			if mErr != nil {
				t.Fatalf("marshal block value: %v", mErr)
			}
			v.Value = raw
		}
		views = append(views, v)
	}

	type diagView struct {
		BlockIndex int    `json:"block_index"`
		Code       string `json:"code"`
		IssueCount int    `json:"issue_count"`
	}
	dv := make([]diagView, 0, len(diags))
	for _, d := range diags {
		dv = append(dv, diagView{BlockIndex: d.BlockIndex, Code: d.Code, IssueCount: len(d.Issues)})
	}

	type capView struct {
		Name       string `json:"name"`
		Consumable bool   `json:"consumable"`
		Emittable  bool   `json:"emittable"`
	}
	var caps []capView
	for _, bt := range r.Types() {
		c := bt.Capabilities()
		caps = append(caps, capView{
			Name:       bt.Name(),
			Consumable: c&Consumable != 0,
			Emittable:  c&Emittable != 0,
		})
	}

	renders := map[string]string{}
	renderInputs := map[string]any{
		"text":        textPayload{Text: "hello world"},
		"preference":  preferencePayload{Name: "style", Text: "Always be kind"},
		"tool_call":   toolCallPayload{CallID: "call_1", Name: "getWeather", Arguments: json.RawMessage(`{"city":"Nairobi"}`)},
		"tool_result": toolResultPayload{CallID: "call_1", Output: json.RawMessage(`{"temp":25}`)},
		"json":        map[string]any{"a": 1},
	}
	for name, in := range renderInputs {
		bt, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("type %q missing", name)
		}
		s, rErr := bt.Render(in)
		if rErr != nil {
			t.Fatalf("Render %s: %v", name, rErr)
		}
		renders[name] = s
	}

	golden := map[string]any{
		"parse":   map[string]any{"blocks": views, "diagnostics": dv},
		"caps":    caps,
		"renders": renders,
	}
	got, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	dir := filepath.Join("..", "testdata", "golden", "registry")
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "registry.json"), got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", filepath.Join(dir, "registry.json"))
		return
	}
	want, err := os.ReadFile(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("read golden (run `go test ./content -run TestGoldenRegistry -update` first): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
