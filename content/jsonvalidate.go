// JSON value validation against the JSON Schema subset the converter emits.
//
// The workspace never accepts arbitrary JSON Schema: the only producer of
// schemas is JSONSchema (see jsonschema.go), so validation only has to
// support the keywords that converter can emit — type (including the
// ["x","null"] nullable form), enum, required, properties,
// additionalProperties (as a value-typed node, the record mapping) and
// items. Anything else is ignored, which matters only if a caller supplies
// a hand-written schema: supported keywords still check honestly, unknown
// ones simply do not constrain.
package content

import (
	"encoding/json"
	"fmt"
	"math"
)

// ValidateJSON reports whether a decoded JSON value satisfies a JSON Schema
// document. The value must come from json.Unmarshal into some any-typed
// decode (map[string]any, []any, scalars); use DecodeJSONValue when the
// value is still raw bytes. The error names the path inside the document.
//
// Validation is against the emitted subset, which is why a schema that
// came through JSONSchema validates faithfully while a foreign schema is
// checked only on the keywords we know. Either way a passing value did
// satisfy every constraint we looked at.
func ValidateJSON(schema json.RawMessage, value any) error {
	var node map[string]any
	if err := json.Unmarshal(schema, &node); err != nil {
		return fmt.Errorf("schema is not JSON: %w", err)
	}
	return validateNode(node, value, "")
}

// DecodeJSONValue decodes raw bytes into the generic shape ValidateJSON
// expects and validates it in one step.
func DecodeJSONValue(schema json.RawMessage, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("value is not JSON: %w", err)
	}
	return ValidateJSON(schema, value)
}

// validateNode checks one value against one schema node.
func validateNode(node map[string]any, value any, path string) error {
	if node == nil {
		return nil
	}
	if types, ok := node["type"]; ok {
		if err := validateType(types, value, path); err != nil {
			return err
		}
		// A nil-typed check is all this node adds.
	}
	if enum, ok := node["enum"].([]any); ok && len(enum) > 0 {
		if !memberOf(enum, value) {
			return fmt.Errorf("%sis not one of the allowed values", at(path))
		}
	}
	switch v := value.(type) {
	case map[string]any:
		return validateObject(node, v, path)
	case []any:
		if items, ok := node["items"].(map[string]any); ok {
			for i, el := range v {
				if err := validateNode(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateType checks the declared type, given as a string or — the
// converter's nullable shape — an array of strings.
func validateType(types any, value any, path string) error {
	check := func(t string) bool {
		switch t {
		case "string":
			_, ok := value.(string)
			return ok
		case "boolean":
			_, ok := value.(bool)
			return ok
		case "number":
			_, ok := value.(float64)
			return ok
		case "integer":
			n, ok := value.(float64)
			return ok && n == math.Trunc(n)
		case "array":
			_, ok := value.([]any)
			return ok
		case "object":
			_, ok := value.(map[string]any)
			return ok
		case "null":
			return value == nil
		}
		return true // an unknown type name constrains nothing
	}
	switch t := types.(type) {
	case string:
		if !check(t) {
			return typeMismatch(value, t, path)
		}
	case []any:
		ok := false
		for _, raw := range t {
			name, isStr := raw.(string)
			if isStr && check(name) {
				ok = true
				break
			}
		}
		if !ok {
			return typeMismatch(value, fmt.Sprint(t), path)
		}
	}
	return nil
}

// validateObject checks required and property constraints for an object
// value. additionalProperties appears only as a record's value node, so it
// validates extra keys instead of forbidding them.
func validateObject(node map[string]any, v map[string]any, path string) error {
	required, _ := node["required"].([]any)
	for _, raw := range required {
		name, ok := raw.(string)
		if ok && !hasKey(v, name) {
			return fmt.Errorf("%smissing required field %q", at(path), name)
		}
	}
	props, _ := node["properties"].(map[string]any)
	for name, value := range v {
		if prop, ok := props[name].(map[string]any); ok {
			if err := validateNode(prop, value, joinPath(path, name)); err != nil {
				return err
			}
			continue
		}
		if extra, ok := node["additionalProperties"].(map[string]any); ok {
			if err := validateNode(extra, value, joinPath(path, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func typeMismatch(value any, want, path string) error {
	return fmt.Errorf("%sexpected %s, got %s", at(path), want, jsonKind(value))
}

func jsonKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// memberOf compares by canonical JSON so numbers and strings do not cross.
func memberOf(enum []any, value any) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	for _, candidate := range enum {
		c, err := json.Marshal(candidate)
		if err != nil {
			continue
		}
		if string(raw) == string(c) {
			return true
		}
	}
	return false
}

func hasKey(v map[string]any, name string) bool {
	_, ok := v[name]
	return ok
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func at(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}
