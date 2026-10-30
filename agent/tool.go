package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/kernel"
)

// NewTool derives the tool's input schema from the input type's struct tags,
// so a tool is a typed function plus a spec.
//
// # Schema derivation goes through anansi, not a second schema system
//
// The derivation is deliberately two-step and both steps live in content:
// reflect the Go type into an anansi definition.Schema, then convert that to
// JSON Schema. It is NOT a hand-rolled JSON Schema writer, because anansi
// already owns schema description and validation here (decision D12), and a
// second independent schema system would duplicate that responsibility.
// content.JSONSchema is the single anansi-to-JSON-Schema translation; its
// package comment carries the full note on the duplication this avoids, and
// on the disposable reflection half that stands in for anansi's off-limits
// extractor.
//
// An input type the derivation cannot describe panics here rather than
// returning an error: the signature returns a Tool, and a tool that cannot
// describe itself is a startup bug, not a runtime condition. That matches
// kernel.Register panicking on a nil handler.
func NewTool[In, Out any](spec ToolSpec, fn func(ctx context.Context, p kernel.Principal, in In) (Out, error)) Tool {
	if spec.Name == "" {
		panic("agent: NewTool called with an empty tool name")
	}
	if fn == nil {
		panic("agent: NewTool called with a nil function")
	}
	if len(spec.Schema) == 0 {
		// The chain is Go type -> anansi schema -> JSON Schema, both halves
		// living in content. See that package's schema-duplication note.
		schema, err := content.JSONSchemaFor(reflect.TypeFor[In]())
		if err != nil {
			panic(fmt.Sprintf("agent: tool %q cannot describe its input: %v", spec.Name, err))
		}
		spec.Schema = schema
	}

	return toolImpl{
		spec: spec,
		call: func(ctx context.Context, p kernel.Principal, args json.RawMessage) (json.RawMessage, error) {
			var in In
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, kernel.WrapError(kernel.CodeInvalid, "tool arguments do not decode", err).
						WithOperation(spec.Name)
				}
			}
			out, err := fn(ctx, p, in)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(out)
			if err != nil {
				return nil, kernel.WrapError(kernel.CodeInternal, "tool result is not JSON-serializable", err).
					WithOperation(spec.Name)
			}
			return raw, nil
		},
	}
}

// toolImpl is the Tool NewTool returns.
type toolImpl struct {
	spec ToolSpec
	call func(ctx context.Context, p kernel.Principal, args json.RawMessage) (json.RawMessage, error)
}

// Spec implements Tool.
func (t toolImpl) Spec() ToolSpec { return t.spec }

// Call implements Tool. Arguments that do not decode are an error result the
// model can see and correct, not a crash; everything else fails the step.
func (t toolImpl) Call(ctx context.Context, p kernel.Principal, call ToolCall) (ToolResult, error) {
	if call.Name != t.spec.Name {
		return ToolResult{}, kernel.NewError(kernel.CodeInvalid,
			"call names "+call.Name+" but this tool is "+t.spec.Name).WithOperation(t.spec.Name)
	}
	out, err := t.call(ctx, p, call.Arguments)
	if err != nil {
		if kernel.IsCode(err, kernel.CodeInvalid) {
			raw, _ := json.Marshal(map[string]any{"error": err.Error()})
			return ToolResult{Output: raw, IsError: true}, nil
		}
		return ToolResult{}, err
	}
	return ToolResult{Output: truncate(out, t.spec.MaxOutput)}, nil
}

// truncate enforces ToolSpec.MaxOutput. A result that does not fit is
// wrapped so it stays valid JSON and is unmistakably flagged, rather than
// being cut into invalid JSON or silently dropped.
func truncate(raw json.RawMessage, max int) json.RawMessage {
	if max <= 0 || len(raw) <= max {
		return raw
	}
	preview := string(raw[:max])
	flagged, err := json.Marshal(map[string]any{
		"truncated": true,
		"bytes":     len(raw),
		"preview":   preview,
	})
	if err != nil {
		return raw[:max]
	}
	return flagged
}
