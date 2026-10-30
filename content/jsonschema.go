// Package content: anansi schema to JSON Schema.
//
// # READ THIS BEFORE ADDING A SECOND SCHEMA SYSTEM
//
// We are duplicating a responsibility that anansi already owns, and this file
// is where that duplication is paid for and contained.
//
// anansi has a full schema system: definition.Schema, typed fields, nested
// schemas, enum values, constraints, and a validator. The workspace uses it
// for block types (D12) and for collection DTOs. Providers, however, do not
// speak anansi — they speak JSON Schema, which is what a tool's arguments and
// a structured output are described with. Two vocabularies for one idea means
// somebody has to translate, and this file is that translation, in one place.
//
// The rule that keeps the duplication from spreading:
//
//	anansi's definition.Schema is the source of truth.
//	JSON Schema is a DERIVED VIEW, produced only by JSONSchema below.
//
// So: never hand-write a JSON Schema where an anansi schema could exist, and
// never add a second converter. If a new caller needs a schema in JSON
// Schema form, feed it an anansi schema and call JSONSchema.
//
// Known gap, tracked so nobody rediscovers it: anansi's own struct-to-schema
// extractor, data.ExtractDTOSchemaDirect, lives in core/data — the persistence
// subtree, which section 5 rules out of every core package. Until it moves to
// a schema-only package, agent.NewTool keeps a small reflection step that
// builds an anansi schema from a Go type and then calls JSONSchema. That
// reflection is the disposable half of this duplication; the converter is the
// durable half. When anansi's extractor becomes importable, delete the
// reflection and keep this file.
//
// See todo/done/agent-loop.md and the "Schema duplication" note in
// todo/done/prompt-pipeline.md.
package content

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
)

// maxSchemaDepth bounds how deep the converter will inline nested schemas.
// Beyond it a node becomes `{}` (any JSON), so a pathological or recursive
// schema degrades instead of hanging.
const maxSchemaDepth = 32

// JSONSchema converts an anansi schema into the JSON Schema a provider
// receives. It is the only place in the workspace that performs this
// translation; see the package comment before writing a second one.
//
// Conversion is lossy in the direction that matters least to a model:
// anansi's indexes, constraints and metadata have no JSON Schema equivalent
// and are dropped. Field types, requiredness, nullability, descriptions,
// defaults and enum values all survive.
//
// Nullability is converted faithfully: anansi treats an absent Nullable flag
// as nullable, so a field with no explicit Nullable=false becomes
// [type, "null"] here. Callers building a schema from a Go type are
// responsible for setting Nullable=false on non-pointer fields; this
// converter will not guess a stricter meaning than the schema states.
func JSONSchema(s *definition.Schema) (json.RawMessage, error) {
	if s == nil {
		return nil, fmt.Errorf("content: nil schema")
	}
	node, err := schemaNode(s, 0)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(node)
	if err != nil {
		return nil, fmt.Errorf("content: schema does not marshal: %w", err)
	}
	return raw, nil
}

// schemaNode renders a whole schema as an object node.
func schemaNode(s *definition.Schema, depth int) (map[string]any, error) {
	if depth > maxSchemaDepth {
		return map[string]any{}, nil
	}
	node := map[string]any{"type": "object"}
	if s.Description != "" {
		node["description"] = s.Description
	}
	props, required, err := fieldsNode(s.Fields, s, depth)
	if err != nil {
		return nil, err
	}
	if len(props) > 0 {
		node["properties"] = props
	}
	if len(required) > 0 {
		node["required"] = required
	}
	return node, nil
}

// fieldsNode renders a field map into properties plus the required list.
func fieldsNode(fields map[definition.FieldId]definition.Field, s *definition.Schema, depth int) (map[string]any, []string, error) {
	props := map[string]any{}
	var required []string
	for _, id := range sortedFieldIDs(fields) {
		f := fields[id]
		node, err := fieldNode(f, s, depth)
		if err != nil {
			return nil, nil, fmt.Errorf("content: field %q: %w", f.Name, err)
		}
		props[string(f.Name)] = node
		if f.Required {
			required = append(required, string(f.Name))
		}
	}
	return props, required, nil
}

// fieldNode renders one field, folding in the inline schema reference when
// the field carries one.
func fieldNode(f definition.Field, s *definition.Schema, depth int) (map[string]any, error) {
	node, err := typeNode(f.Type, s, depth)
	if err != nil {
		return nil, err
	}

	// The field's schema reference refines it. For an array or a record the
	// reference describes the element or value type, inline or named; for
	// anything else an inline reference may simply be a more specific type,
	// and a named one a nested object.
	if ref, ok := schemaRef(f.Schema); ok {
		switch {
		case f.Type == definition.FieldTypeArray:
			items, err := referenceNode(ref, s, depth+1)
			if err != nil {
				return nil, err
			}
			node = map[string]any{"type": "array", "items": items}
		case f.Type == definition.FieldTypeRecord:
			// A record is a map: the reference supplies the value type,
			// which becomes additionalProperties.
			values, err := referenceNode(ref, s, depth+1)
			if err != nil {
				return nil, err
			}
			node = map[string]any{"type": "object", "additionalProperties": values}
		case ref.IsInline():
			refined, err := referenceNode(ref, s, depth+1)
			if err != nil {
				return nil, err
			}
			// Keep the field's own type when the reference adds no type of
			// its own; otherwise the reference is more specific.
			// referenceNode only sets "enum" when there are values, so key
			// presence is enough.
			_, hasType := refined["type"]
			_, hasEnum := refined["enum"]
			if hasType || hasEnum {
				node = refined
			}
		case ref.ID != "":
			if nested, found := s.Schemas[ref.ID]; found {
				inner, err := nestedNode(nested, s, depth+1)
				if err != nil {
					return nil, err
				}
				node = inner
			}
		}
	}

	if f.Description != "" {
		node["description"] = f.Description
	}
	if !f.Default.IsZero() && !f.Default.IsNull() {
		node["default"] = f.Default.Value()
	}
	// A nullable field accepts null alongside its type. ResolvedNullable
	// treats an absent flag as nullable, which matches the JSON Schema
	// default of allowing null unless stated otherwise.
	if f.ResolvedNullable() {
		node = withNull(node)
	}
	return node, nil
}

// referenceNode renders a schema reference: a named nested schema resolved
// through the parent, an enum's values, or an inline type.
func referenceNode(ref definition.SchemaReference, s *definition.Schema, depth int) (map[string]any, error) {
	if depth > maxSchemaDepth {
		return map[string]any{}, nil
	}
	// A named reference — an array of structs, say — resolves to the nested
	// schema's own rendering.
	if ref.ID != "" {
		if nested, ok := s.Schemas[ref.ID]; ok {
			return nestedNode(nested, s, depth+1)
		}
	}
	if len(ref.Values) > 0 {
		values := make([]any, 0, len(ref.Values))
		for _, v := range ref.Values {
			values = append(values, v.Value())
		}
		return map[string]any{"enum": values}, nil
	}
	if ref.Type == definition.FieldTypeUnknown {
		return map[string]any{}, nil
	}
	return typeNode(ref.Type, s, depth)
}

// nestedNode renders a nested schema, which is a schema plus field
// properties of its own.
func nestedNode(ns definition.NestedSchema, s *definition.Schema, depth int) (map[string]any, error) {
	if depth > maxSchemaDepth {
		return map[string]any{}, nil
	}
	if len(ns.Values) > 0 {
		values := make([]any, 0, len(ns.Values))
		for _, v := range ns.Values {
			values = append(values, v.Value())
		}
		return map[string]any{"enum": values}, nil
	}
	if len(ns.Fields) > 0 {
		return schemaNode(&definition.Schema{BaseSchema: ns.BaseSchema, Schemas: s.Schemas}, depth)
	}
	if ns.Type != definition.FieldTypeUnknown {
		return typeNode(ns.Type, s, depth)
	}
	return map[string]any{}, nil
}

// typeNode maps one anansi field type onto a JSON Schema node.
func typeNode(t definition.FieldType, s *definition.Schema, depth int) (map[string]any, error) {
	if depth > maxSchemaDepth {
		return map[string]any{}, nil
	}
	switch t {
	case definition.FieldTypeString, definition.FieldTypeGeometry:
		// Geometry is a string in JSON Schema terms (WKT or GeoJSON as text).
		return map[string]any{"type": "string"}, nil
	case definition.FieldTypeNumber, definition.FieldTypeDecimal:
		// Decimal is a number with more precision than a float64 carries;
		// JSON Schema can only say "number".
		return map[string]any{"type": "number"}, nil
	case definition.FieldTypeInteger:
		return map[string]any{"type": "integer"}, nil
	case definition.FieldTypeBoolean:
		return map[string]any{"type": "boolean"}, nil
	case definition.FieldTypeBytes:
		return map[string]any{"type": "string", "contentEncoding": "base64"}, nil
	case definition.FieldTypeArray:
		return map[string]any{"type": "array", "items": map[string]any{}}, nil
	case definition.FieldTypeObject, definition.FieldTypeRecord:
		return map[string]any{"type": "object"}, nil
	case definition.FieldTypeEnum:
		// Values arrive through an inline or named reference, not the type.
		return map[string]any{}, nil
	case definition.FieldTypeUnion, definition.FieldTypeComposite, definition.FieldTypeUnknown:
		// No single JSON Schema type expresses these; any JSON is the safe
		// reading, and a caller that needs precision authors the schema.
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("unsupported anansi field type %d", t)
	}
}

// withNull widens a node to accept null, which is how anansi's nullable
// fields read in JSON Schema.
func withNull(node map[string]any) map[string]any {
	t, ok := node["type"].(string)
	if !ok {
		return node
	}
	out := make(map[string]any, len(node)+1)
	for k, v := range node {
		out[k] = v
	}
	out["type"] = []string{t, "null"}
	return out
}

// schemaRef unwraps a field's schema reference, which is either a single
// SchemaReference or a list of them. Only the single form is used here; a
// list is a union, which JSON Schema expresses as anyOf and which the
// converter currently degrades to any.
func schemaRef(fr definition.FieldSchemaReference) (definition.SchemaReference, bool) {
	if fr.IsZero() {
		return definition.SchemaReference{}, false
	}
	ref, err := definition.FieldSchemaAs[definition.SchemaReference](fr)
	if err != nil {
		return definition.SchemaReference{}, false
	}
	return ref, true
}

// sortedFieldIDs makes field order deterministic, because map iteration is
// not, and a provider request must be byte-stable for golden tests.
func sortedFieldIDs(fields map[definition.FieldId]definition.Field) []definition.FieldId {
	ids := make([]definition.FieldId, 0, len(fields))
	for id := range fields {
		ids = append(ids, id)
	}
	// Sort by field name, then id, so the same schema always renders the same
	// way regardless of how the map was built.
	sort.Slice(ids, func(i, j int) bool {
		a, b := fields[ids[i]], fields[ids[j]]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return ids[i] < ids[j]
	})
	return ids
}
