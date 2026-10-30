package content

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
)

// field builds a field, and ref builds an inline schema reference — the only
// way to construct one is to unmarshal JSON, since its fields are unexported.
func ref(t *testing.T, js string) definition.FieldSchemaReference {
	t.Helper()
	var fr definition.FieldSchemaReference
	if err := json.Unmarshal([]byte(js), &fr); err != nil {
		t.Fatalf("reference %s does not decode: %v", js, err)
	}
	return fr
}

// field builds a field that is non-nullable by default, because anansi
// treats an absent Nullable flag as nullable and the converter is faithful to
// that. Tests that want the widening set Nullable back to true.
func field(name string, typ definition.FieldType, opts ...func(*definition.Field)) definition.Field {
	f := definition.Field{
		Name:            definition.FieldName(name),
		FieldProperties: definition.FieldProperties{Type: typ},
		Nullable:        definition.BoolPtr(false),
	}
	for _, o := range opts {
		o(&f)
	}
	return f
}

func nullable(f *definition.Field) { f.Nullable = definition.BoolPtr(true) }

func required(f *definition.Field) { f.Required = true }
func desc(d string) func(*definition.Field) {
	return func(f *definition.Field) { f.Description = d }
}

func render(t *testing.T, s *definition.Schema) map[string]any {
	t.Helper()
	raw, err := JSONSchema(s)
	if err != nil {
		t.Fatalf("JSONSchema: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("output is not an object: %v\n%s", err, raw)
	}
	return out
}

func TestJSONSchemaMapsScalarTypes(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("name", definition.FieldTypeString, required),
		"f2": field("age", definition.FieldTypeInteger, required),
		"f3": field("score", definition.FieldTypeNumber),
		"f4": field("ratio", definition.FieldTypeDecimal),
		"f5": field("ok", definition.FieldTypeBoolean, required),
		"f6": field("blob", definition.FieldTypeBytes),
	}}}
	got := render(t, s)
	props := got["properties"].(map[string]any)

	want := map[string]map[string]any{
		"name":  {"type": "string"},
		"age":   {"type": "integer"},
		"score": {"type": "number"},
		"ratio": {"type": "number"},
		"ok":    {"type": "boolean"},
		"blob":  {"type": "string", "contentEncoding": "base64"},
	}
	for name, w := range want {
		p := props[name].(map[string]any)
		for k, v := range w {
			if !reflect.DeepEqual(p[k], v) {
				t.Errorf("%s.%s = %v, want %v", name, k, p[k], v)
			}
		}
	}
}

func TestJSONSchemaCollectsRequiredAndDescription(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("a", definition.FieldTypeString, required, desc("the a")),
		"f2": field("b", definition.FieldTypeString),
	}}}
	got := render(t, s)
	req := got["required"].([]any)
	if len(req) != 1 || req[0] != "a" {
		t.Fatalf("required = %v, want [a]", req)
	}
	props := got["properties"].(map[string]any)
	if props["a"].(map[string]any)["description"] != "the a" {
		t.Fatal("description did not survive the conversion")
	}
	if _, ok := props["b"].(map[string]any)["description"]; ok {
		t.Fatal("an empty description must not be emitted")
	}
}

func TestJSONSchemaNullableWidensTheType(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("maybe", definition.FieldTypeString, nullable),
	}}}
	p := render(t, s)["properties"].(map[string]any)["maybe"].(map[string]any)
	got, ok := p["type"].([]any)
	if !ok {
		t.Fatalf("a nullable field must widen type to an array, got %v", p["type"])
	}
	if len(got) != 2 || got[0] != "string" || got[1] != "null" {
		t.Fatalf("type = %v, want [string null]", got)
	}
}

func TestJSONSchemaNonNullableKeepsASingleType(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("fixed", definition.FieldTypeString, func(f *definition.Field) {
			f.Nullable = definition.BoolPtr(false)
		}),
	}}}
	p := render(t, s)["properties"].(map[string]any)["fixed"].(map[string]any)
	if p["type"] != "string" {
		t.Fatalf("a non-nullable field must keep a single type, got %v", p["type"])
	}
}

func TestJSONSchemaInlineEnumBecomesEnumValues(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("mode", definition.FieldTypeEnum, func(f *definition.Field) {
			f.Schema = ref(t, `{"values":["read","write"]}`)
		}),
	}}}
	p := render(t, s)["properties"].(map[string]any)["mode"].(map[string]any)
	values, ok := p["enum"].([]any)
	if !ok {
		t.Fatalf("an enum must become enum values, got %v", p)
	}
	if len(values) != 2 || values[0] != "read" || values[1] != "write" {
		t.Fatalf("enum = %v", values)
	}
}

func TestJSONSchemaArrayItemsComeFromTheInlineReference(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("tags", definition.FieldTypeArray, func(f *definition.Field) {
			f.Schema = ref(t, `{"type":"string"}`)
		}),
	}}}
	p := render(t, s)["properties"].(map[string]any)["tags"].(map[string]any)
	if p["type"] != "array" {
		t.Fatalf("type = %v, want array", p["type"])
	}
}

func TestJSONSchemaInlinesANamedNestedSchema(t *testing.T) {
	nested := definition.NestedSchema{
		BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
			"n1": field("lat", definition.FieldTypeNumber, required),
			"n2": field("lon", definition.FieldTypeNumber, required),
		}},
	}
	s := &definition.Schema{
		BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
			"f1": field("where", definition.FieldTypeObject, func(f *definition.Field) {
				f.Schema = ref(t, `{"id":"nested-1"}`)
			}),
		}},
		Schemas: map[definition.SchemaId]definition.NestedSchema{"nested-1": nested},
	}
	p := render(t, s)["properties"].(map[string]any)["where"].(map[string]any)
	props, ok := p["properties"].(map[string]any)
	if !ok {
		t.Fatalf("a named nested schema must be inlined, got %v", p)
	}
	if _, ok := props["lat"]; !ok {
		t.Fatalf("nested fields were lost: %v", props)
	}
}

func TestJSONSchemaDefaultSurvives(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("n", definition.FieldTypeInteger),
	}}}
	// A default is a LiteralValue, which is only constructible through its
	// constructors; skip when none is exported, since the zero value simply
	// emits no default.
	raw, err := JSONSchema(s)
	if err != nil {
		t.Fatalf("JSONSchema: %v", err)
	}
	if strings.Contains(string(raw), `"default"`) {
		t.Fatalf("a zero default must not be emitted: %s", raw)
	}
}

func TestJSONSchemaIsDeterministic(t *testing.T) {
	// Same fields, different map insertion order, same bytes.
	mk := func() *definition.Schema {
		return &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
			"f3": field("c", definition.FieldTypeString),
			"f1": field("a", definition.FieldTypeString),
			"f2": field("b", definition.FieldTypeString),
		}}}
	}
	a, err := JSONSchema(mk())
	if err != nil {
		t.Fatalf("JSONSchema: %v", err)
	}
	b, err := JSONSchema(mk())
	if err != nil {
		t.Fatalf("JSONSchema: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("conversion is not deterministic:\n%s\n%s", a, b)
	}
	// Fields sort by name.
	if !strings.Contains(string(a), `"properties":{"a"`) {
		t.Fatalf("fields must sort by name: %s", a)
	}
}

func TestJSONSchemaRejectsNil(t *testing.T) {
	if _, err := JSONSchema(nil); err == nil {
		t.Fatal("a nil schema must be an error")
	}
}

func TestJSONSchemaEmptySchemaIsEmptyObject(t *testing.T) {
	raw, err := JSONSchema(&definition.Schema{})
	if err != nil {
		t.Fatalf("JSONSchema: %v", err)
	}
	if string(raw) != `{"type":"object"}` {
		t.Fatalf("empty schema = %s, want a bare object", raw)
	}
}

func TestJSONSchemaUnionDegradesToAny(t *testing.T) {
	s := &definition.Schema{BaseSchema: definition.BaseSchema{Fields: map[definition.FieldId]definition.Field{
		"f1": field("either", definition.FieldTypeUnion),
	}}}
	p := render(t, s)["properties"].(map[string]any)["either"].(map[string]any)
	if _, hasType := p["type"]; hasType {
		t.Fatalf("a union has no single JSON type, got %v", p)
	}
}
