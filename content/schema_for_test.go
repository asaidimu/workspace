package content

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSchemaForRejectsNonStructsAndNil(t *testing.T) {
	if _, err := SchemaFor(nil); err == nil {
		t.Fatal("a nil type must be an error")
	}
	if _, err := SchemaFor(reflect.TypeFor[string]()); err == nil {
		t.Fatal("a non-struct root must be an error")
	}
	if _, err := SchemaFor(reflect.TypeFor[*string]()); err == nil {
		t.Fatal("a pointer to a non-struct must be an error")
	}
}

func TestSchemaForSkipsUnexportedAndDashedFields(t *testing.T) {
	type s struct {
		Kept string `json:"kept"`
		Hide string `json:"-"`
		also string
	}
	sch, err := SchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	if len(sch.Fields) != 1 {
		t.Fatalf("fields = %d, want 1", len(sch.Fields))
	}
	for _, f := range sch.Fields {
		if f.Name != "kept" {
			t.Fatalf("kept field is %q", f.Name)
		}
	}
}

func TestSchemaForMarksPointersNullableAndValueFieldsRequired(t *testing.T) {
	type s struct {
		Must   string  `json:"must"`
		Maybe  *string `json:"maybe"`
		Seldom string  `json:"seldom,omitempty"`
	}
	sch, err := SchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	byName := map[string]bool{}
	for _, f := range sch.Fields {
		byName[string(f.Name)] = f.Required
	}
	if !byName["must"] {
		t.Error("a value field without omitempty must be required")
	}
	if byName["maybe"] {
		t.Error("a pointer field must not be required")
	}
	if byName["seldom"] {
		t.Error("an omitempty field must not be required")
	}

	raw, err := JSONSchema(sch)
	if err != nil {
		t.Fatalf("JSONSchema: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output: %v", err)
	}
	props := out["properties"].(map[string]any)
	if _, isList := props["maybe"].(map[string]any)["type"].([]any); !isList {
		t.Errorf("a pointer field must widen to [type null], got %v", props["maybe"])
	}
	if _, isList := props["must"].(map[string]any)["type"].([]any); isList {
		t.Errorf("a value field must keep a single type, got %v", props["must"])
	}
}

func TestJSONSchemaForMapValuesBecomeAdditionalProperties(t *testing.T) {
	type s struct {
		Meta map[string]int `json:"meta"`
	}
	raw, err := JSONSchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("JSONSchemaFor: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output: %v", err)
	}
	meta := out["properties"].(map[string]any)["meta"].(map[string]any)
	addl, ok := meta["additionalProperties"].(map[string]any)
	if !ok {
		t.Fatalf("a map must carry its value type as additionalProperties: %s", raw)
	}
	if addl["type"] != "integer" {
		t.Fatalf("additionalProperties = %v, want integer", addl)
	}
}

func TestSchemaForRejectsNonStringMapKeys(t *testing.T) {
	type s struct {
		M map[int]string `json:"m"`
	}
	if _, err := SchemaFor(reflect.TypeFor[s]()); err == nil {
		t.Fatal("non-string map keys must be rejected")
	}
}

func TestSchemaForHandlesTimeAndRawJSON(t *testing.T) {
	type s struct {
		At   time.Time       `json:"at"`
		Body json.RawMessage `json:"body"`
	}
	raw, err := JSONSchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("JSONSchemaFor: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output: %v", err)
	}
	props := out["properties"].(map[string]any)
	if props["at"].(map[string]any)["type"] != "string" {
		t.Errorf("time.Time must become a string, got %v", props["at"])
	}
	// json.RawMessage is "any", which has no type.
	if _, hasType := props["body"].(map[string]any)["type"]; hasType {
		t.Errorf("json.RawMessage must stay untyped, got %v", props["body"])
	}
}

func TestSchemaForNestedStructsAreInlined(t *testing.T) {
	type inner struct {
		Deep string `json:"deep"`
	}
	type outer struct {
		Inner inner `json:"inner"`
	}
	raw, err := JSONSchemaFor(reflect.TypeFor[outer]())
	if err != nil {
		t.Fatalf("JSONSchemaFor: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output: %v", err)
	}
	got := out["properties"].(map[string]any)["inner"].(map[string]any)
	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatalf("a nested struct must inline its fields: %s", raw)
	}
	if _, ok := props["deep"]; !ok {
		t.Fatalf("nested field lost: %s", raw)
	}
}

func TestSchemaForArraysOfStructsResolveTheNestedSchema(t *testing.T) {
	type item struct {
		Name string `json:"name"`
	}
	type s struct {
		Items []item `json:"items"`
	}
	raw, err := JSONSchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("JSONSchemaFor: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output: %v", err)
	}
	items := out["properties"].(map[string]any)["items"].(map[string]any)
	if items["type"] != "array" {
		t.Fatalf("items must stay an array, got %s", raw)
	}
	elem, ok := items["items"].(map[string]any)
	if !ok {
		t.Fatalf("items must carry its element shape: %s", raw)
	}
	if elem["type"] != "object" {
		t.Fatalf("the element must resolve to the nested object, got %v", elem)
	}
	props, ok := elem["properties"].(map[string]any)
	if !ok {
		t.Fatalf("nested fields lost: %s", raw)
	}
	if _, ok := props["name"]; !ok {
		t.Fatalf("nested field lost: %s", raw)
	}
}

func TestSchemaForIsDeterministic(t *testing.T) {
	type s struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	a, err := JSONSchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("JSONSchemaFor: %v", err)
	}
	b, err := JSONSchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("JSONSchemaFor: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("derivation is not deterministic:\n%s\n%s", a, b)
	}
	if !strings.Contains(string(a), `"properties":{"a"`) {
		t.Fatalf("fields must sort by name: %s", a)
	}
}

func TestSchemaForFieldIDsAreStableUUIDs(t *testing.T) {
	type s struct {
		A string `json:"a"`
	}
	first, err := SchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	second, err := SchemaFor(reflect.TypeFor[s]())
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	for id := range first.Fields {
		if _, ok := second.Fields[id]; !ok {
			t.Fatalf("field id %q was not stable across derivations", id)
		}
		if len(id) != 36 {
			t.Fatalf("field id %q is not a UUID", id)
		}
	}
}
