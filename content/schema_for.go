package content

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
)

// This file is the other half of the schema-duplication note in
// jsonschema.go. There, JSON Schema is a derived view of an anansi schema.
// Here, a Go type becomes an anansi schema, so the derivation chain is
//
//	Go type -> definition.Schema -> JSON Schema
//
// with exactly one converter and exactly one reflector, both in this
// package, which is the one place section 5 allows anansi's schema packages
// into the core.
//
// The reflection is a stand-in for anansi's own extractor,
// data.ExtractDTOSchemaDirect, which lives in core/data — the persistence
// subtree, out of reach of every core package. When that extractor moves
// somewhere importable, delete SchemaFor and keep JSONSchema.

// @note #shzrsh issue : Duplication of effort
// @see #7wsf3d
//
// This implementation, which attempts to derive an anansi schema for
// any go type should be rightfully moved into the anansi library itself.
// Otherwise, in this codebase, we should prefer defining anansi tags for our
// content

// maxDeriveDepth bounds how deep reflection follows nested structs.
const maxDeriveDepth = 16

// SchemaFor reflects a Go struct type into an anansi schema. It covers the
// kinds a tool argument realistically uses: structs, pointers, slices, maps,
// the scalar types, time.Time and json.RawMessage. Anything else is
// rejected, and a non-struct top level is an error because a schema's root
// is an object.
func SchemaFor(t reflect.Type) (*definition.Schema, error) {
	if t == nil {
		return nil, fmt.Errorf("content: nil type")
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("content: schema root must be a struct, got %s", t.Kind())
	}
	s := &definition.Schema{
		BaseSchema: definition.BaseSchema{Name: "arguments"},
		Schemas:    map[definition.SchemaId]definition.NestedSchema{},
	}
	fields, err := anansiFields(s, t, 0)
	if err != nil {
		return nil, err
	}
	s.Fields = fields
	return s, nil
}

// JSONSchemaFor is the whole chain: reflect the type, then convert. Callers
// that only want the provider-facing bytes use this; callers that want to
// inspect or extend the anansi schema first use SchemaFor and JSONSchema.
func JSONSchemaFor(t reflect.Type) (json.RawMessage, error) {
	s, err := SchemaFor(t)
	if err != nil {
		return nil, err
	}
	return JSONSchema(s)
}

// anansiFields reflects one struct's exported fields into anansi fields,
// registering nested structs as nested schemas on s.
func anansiFields(s *definition.Schema, t reflect.Type, depth int) (map[definition.FieldId]definition.Field, error) {
	if depth > maxDeriveDepth {
		return nil, fmt.Errorf("content: type nests more than %d levels", maxDeriveDepth)
	}
	fields := map[definition.FieldId]definition.Field{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, opts := jsonName(sf)
		if name == "-" {
			continue
		}

		isPointer := sf.Type.Kind() == reflect.Pointer
		inner := sf.Type
		if isPointer {
			inner = inner.Elem()
		}

		ft, ref, err := anansiType(s, inner, depth)
		if err != nil {
			return nil, fmt.Errorf("content: field %s: %w", sf.Name, err)
		}
		// Go's nullability is the schema's nullability: a pointer field may
		// be absent, a value field may not. JSONSchema widens a nullable
		// field to [type, "null"], so this is where the Go meaning becomes
		// the schema's.
		fields[fieldID(t, sf)] = definition.Field{
			Name:     definition.FieldName(name),
			Required: !isPointer && !opts["omitempty"],
			Nullable: definition.BoolPtr(isPointer),
			FieldProperties: definition.FieldProperties{
				Type:   ft,
				Schema: ref,
			},
		}
	}
	return fields, nil
}

// anansiType maps one Go type onto an anansi field type plus an optional
// inline or named reference carrying the element, value or nested-object
// shape.
func anansiType(s *definition.Schema, t reflect.Type, depth int) (definition.FieldType, definition.FieldSchemaReference, error) {
	var zero definition.FieldSchemaReference

	switch {
	case t == reflect.TypeFor[time.Time]():
		return definition.FieldTypeString, zero, nil
	case t == reflect.TypeFor[json.RawMessage]():
		// Any JSON. anansi has no "any", so an untyped field it is.
		return definition.FieldTypeUnknown, zero, nil
	}

	switch t.Kind() {
	case reflect.String:
		return definition.FieldTypeString, zero, nil
	case reflect.Bool:
		return definition.FieldTypeBoolean, zero, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return definition.FieldTypeInteger, zero, nil
	case reflect.Float32, reflect.Float64:
		return definition.FieldTypeNumber, zero, nil
	case reflect.Slice, reflect.Array:
		elemType, elemRef, err := anansiType(s, t.Elem(), depth+1)
		if err != nil {
			return 0, zero, err
		}
		// A scalar element carries no reference of its own, so build one
		// that states the element type; without it the array would have no
		// items type.
		if elemRef.IsZero() {
			elemRef, err = refForType(elemType)
			if err != nil {
				return 0, zero, err
			}
		}
		return definition.FieldTypeArray, elemRef, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return 0, zero, fmt.Errorf("content: map keys must be strings, got %s", t.Key())
		}
		valueType, valueRef, err := anansiType(s, t.Elem(), depth+1)
		if err != nil {
			return 0, zero, err
		}
		if valueRef.IsZero() {
			valueRef, err = refForType(valueType)
			if err != nil {
				return 0, zero, err
			}
		}
		return definition.FieldTypeRecord, valueRef, nil
	case reflect.Struct:
		// A nested struct becomes a named nested schema, referenced by id, so
		// a repeated shape is described once.
		nested, err := anansiFields(s, t, depth+1)
		if err != nil {
			return 0, zero, err
		}
		id := nestedID(t)
		s.Schemas[id] = definition.NestedSchema{
			BaseSchema: definition.BaseSchema{Name: t.Name(), Fields: nested},
		}
		ref, err := refForID(id)
		if err != nil {
			return 0, zero, err
		}
		return definition.FieldTypeObject, ref, nil
	case reflect.Interface:
		return definition.FieldTypeUnknown, zero, nil
	default:
		return 0, zero, fmt.Errorf("content: unsupported kind %s", t.Kind())
	}
}

// refForID builds a named schema reference. FieldSchemaReference's fields are
// unexported, so the only way to construct one is to round-trip its JSON.
func refForID(id definition.SchemaId) (definition.FieldSchemaReference, error) {
	var zero definition.FieldSchemaReference
	raw, err := json.Marshal(definition.SchemaReference{ID: id})
	if err != nil {
		return zero, err
	}
	var fr definition.FieldSchemaReference
	if err := json.Unmarshal(raw, &fr); err != nil {
		return zero, err
	}
	return fr, nil
}

// refForType builds an inline reference that states a field type, which is
// how an array's element type and a map's value type are carried.
func refForType(t definition.FieldType) (definition.FieldSchemaReference, error) {
	var zero definition.FieldSchemaReference
	raw, err := json.Marshal(definition.SchemaReference{Type: t})
	if err != nil {
		return zero, err
	}
	var fr definition.FieldSchemaReference
	if err := json.Unmarshal(raw, &fr); err != nil {
		return zero, err
	}
	return fr, nil
}

// fieldID mints a deterministic id for a field. These schemas are transient —
// they describe a tool's arguments and are never created as an anansi
// collection — so the UUIDv7 requirement anansi enforces on persisted
// schemas does not apply, and a deterministic id is worth more here because
// it keeps the derived JSON Schema byte-stable across registrations.
func fieldID(owner reflect.Type, f reflect.StructField) definition.FieldId {
	return definition.FieldId(uuid.NewSHA1(uuid.NameSpaceOID, []byte(owner.String()+"."+f.Name)).String())
}

func nestedID(t reflect.Type) definition.SchemaId {
	return definition.SchemaId(uuid.NewSHA1(uuid.NameSpaceOID, []byte("nested:"+t.String())).String())
}

// jsonName reads the field's JSON name and options, falling back to the field
// name so a struct without tags still describes itself.
func jsonName(f reflect.StructField) (string, map[string]bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, map[string]bool{}
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = f.Name
	}
	opts := map[string]bool{}
	for _, o := range parts[1:] {
		opts[o] = true
	}
	return name, opts
}
