package gemini

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/provider/internal/recordtest"
)

// TestBuildBodyFlattensNullableSchemas guards the Gemini Schema proto, which
// rejects `"type": ["string","null"]` with INVALID_ARGUMENT. The workspace's
// converter emits exactly that for nullable fields, so tool and output
// schemas are flattened before they reach the wire.
func TestBuildBodyFlattensNullableSchemas(t *testing.T) {
	c := New(recordtest.FixedCreds("test-key"))
	req := provider.Request{
		Model: "gemini-2.5-flash",
		Tools: []provider.ToolSpec{{
			Name: "shell",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string"},
					"workdir": {"type": ["string", "null"]},
					"opts": {"type": "object", "properties": {"dir": {"type": ["string", "null"]}}},
					"list": {"type": "array", "items": {"type": ["string", "null"]}}
				},
				"required": ["command"]
			}`),
		}},
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":["string","null"]}}}`),
	}

	body, err := c.buildBody(context.Background(), req, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	tools := body["tools"].([]map[string]any)
	decls := tools[0]["functionDeclarations"].([]map[string]any)
	params := decls[0]["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	checkType(t, props["workdir"], "string", "workdir")
	opts := props["opts"].(map[string]any)["properties"].(map[string]any)
	checkType(t, opts["dir"], "string", "opts.dir")
	items := props["list"].(map[string]any)["items"]
	checkType(t, items, "string", "list.items")

	gen := body["generationConfig"].(map[string]any)
	response := gen["responseSchema"].(map[string]any)
	responseProps := response["properties"].(map[string]any)
	checkType(t, responseProps["note"], "string", "responseSchema.note")

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	if got := string(raw); containsNullType(got) {
		t.Errorf("body still carries a nullable type array:\n%s", got)
	}
}

func checkType(t *testing.T, node any, want, path string) {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want an object", path, node)
	}
	if got := m["type"]; got != want {
		t.Errorf("%s type = %v, want %q", path, got, want)
	}
}

func containsNullType(s string) bool {
	for i := 0; i+7 <= len(s); i++ {
		if s[i:i+7] == `["strin` {
			return true
		}
	}
	return false
}
