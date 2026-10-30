package content

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/asaidimu/go-anansi/v8/core/common"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
)

// ParseOptions configures text and provider parts parsing.
type ParseOptions struct {
	Provider    string
	NativeTools bool
}

// openFenceRegex matches an opening fence line:
// ```workspace or ```workspace:<type>
var openFenceRegex = regexp.MustCompile(`^\s*` + "```" + `workspace(?::([A-Za-z0-9_.:-]+))?\s*$`)

// closeFenceRegex matches a closing fence line:
// ```
var closeFenceRegex = regexp.MustCompile(`^\s*` + "```" + `\s*$`)

// ParseText parses ordinary prose with embedded ```workspace:<type> or
// ```workspace code blocks.
func (r *registry) ParseText(text string) ([]domain.Block, []Diagnostic, error) {
	return parseTextWithRegistry(r, text, ParseOptions{})
}

// parseTextWithRegistry parses markdown text into blocks using the given registry and options.
func parseTextWithRegistry(r Registry, text string, opts ParseOptions) ([]domain.Block, []Diagnostic, error) {
	var blocks []domain.Block
	var diags []Diagnostic

	lines := strings.Split(text, "\n")
	var proseLines []string

	flushProse := func() {
		if len(proseLines) == 0 {
			return
		}
		proseText := strings.Join(proseLines, "\n")
		proseLines = nil
		if strings.TrimSpace(proseText) == "" {
			return
		}
		blocks = append(blocks, domain.Block{
			Type:  "text",
			Value: textPayload{Text: proseText},
		})
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		submatches := openFenceRegex.FindStringSubmatch(line)
		if submatches == nil {
			proseLines = append(proseLines, line)
			continue
		}

		// Opening fence matched.
		flushProse()
		explicitType := submatches[1]

		// Collect fence body until closing fence or EOF.
		bodyStartIdx := i + 1
		foundClose := false
		var bodyLines []string

		for i = bodyStartIdx; i < len(lines); i++ {
			if closeFenceRegex.MatchString(lines[i]) {
				foundClose = true
				break
			}
			bodyLines = append(bodyLines, lines[i])
		}

		if !foundClose {
			// Unterminated fence: keep remainder as prose and record diagnostic.
			allRemaining := lines[bodyStartIdx-1:]
			proseLines = append(proseLines, allRemaining...)
			diags = append(diags, Diagnostic{
				BlockIndex: len(blocks),
				Code:       "unterminated",
			})
			break
		}

		rawBody := strings.Join(bodyLines, "\n")
		block, blockDiags := parseFencePayload(r, explicitType, rawBody, opts, len(blocks))
		blocks = append(blocks, block)
		diags = append(diags, blockDiags...)
	}

	flushProse()
	return blocks, diags, nil
}

// parseFencePayload interprets the body inside a ```workspace[:type] fence.
func parseFencePayload(r Registry, explicitType string, rawBody string, opts ParseOptions, blockIndex int) (domain.Block, []Diagnostic) {
	rawBytes := []byte(rawBody)

	// Body must be one JSON object with number fidelity.
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(rawBytes))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil || obj == nil {
		diagMsg := "invalid JSON payload"
		if err != nil {
			diagMsg = err.Error()
		}
		return unknownBlock(rawBytes, blockEnvelope{Type: explicitType}), []Diagnostic{{
			BlockIndex: blockIndex,
			Code:       "invalid",
			Issues:     []common.Issue{{Code: "invalid", Message: diagMsg, Severity: "error"}},
		}}
	}

	// Check if additional JSON content remains.
	if dec.More() {
		return unknownBlock(rawBytes, blockEnvelope{Type: explicitType}), []Diagnostic{{
			BlockIndex: blockIndex,
			Code:       "invalid",
			Issues:     []common.Issue{{Code: "invalid", Message: "multiple JSON values in payload", Severity: "error"}},
		}}
	}

	typeName := explicitType
	var payload map[string]any = obj
	var key string

	// Check for envelope form: ```workspace with {"type": "...", "value": ...}
	if typeName == "" {
		if tVal, ok := obj["type"].(string); ok && tVal != "" {
			typeName = tVal
			if kVal, ok := obj["key"].(string); ok {
				key = kVal
			}
			if vVal, ok := obj["value"].(map[string]any); ok {
				payload = vVal
			}
		} else if tVal, ok := obj["Type"].(string); ok && tVal != "" {
			typeName = tVal
			if kVal, ok := obj["Key"].(string); ok {
				key = kVal
			}
			if vVal, ok := obj["Value"].(map[string]any); ok {
				payload = vVal
			}
		} else {
			return unknownBlock(rawBytes, blockEnvelope{Type: ""}), []Diagnostic{{
				BlockIndex: blockIndex,
				Code:       "unknown_type",
			}}
		}
	}

	// If type is tool_call, check native tools constraint.
	if typeName == "tool_call" && opts.NativeTools {
		return unknownBlock(rawBytes, blockEnvelope{Type: typeName, Key: key}), []Diagnostic{{
			BlockIndex: blockIndex,
			Code:       "native_tool_expected",
		}}
	}

	bt, ok := r.Lookup(typeName)
	if !ok {
		return unknownBlock(rawBytes, blockEnvelope{Type: typeName, Key: key}), []Diagnostic{{
			BlockIndex: blockIndex,
			Code:       "unknown_type",
		}}
	}

	// Non-emittable type check: model is not allowed to emit types without Emittable capability.
	if (bt.Capabilities() & Emittable) == 0 {
		return unknownBlock(rawBytes, blockEnvelope{Type: typeName, Key: key}), []Diagnostic{{
			BlockIndex: blockIndex,
			Code:       "non_emittable",
		}}
	}

	value, issues := bt.Decode(payload)
	if value == nil {
		return unknownBlock(rawBytes, blockEnvelope{Type: typeName, Key: key}), []Diagnostic{{
			BlockIndex: blockIndex,
			Code:       "invalid",
			Issues:     issues,
		}}
	}

	valBytes, err := json.Marshal(value)
	if err != nil {
		valBytes = rawBytes
	}

	return domain.Block{
		Type:  typeName,
		Key:   key,
		Value: value,
		Raw:   valBytes,
	}, nil
}

// ParseParts converts provider response parts into typed domain blocks and diagnostics.
// TextPart scans for embedded workspace fences and prose.
// ThinkingPart maps to a thinking block.
// ToolCallPart maps to a tool_call block.
// ImagePart and FilePart map to typed blocks or unknown blocks if unresolvable.
func ParseParts(r Registry, parts []provider.Part, opts ParseOptions) ([]domain.Block, []Diagnostic, error) {
	var blocks []domain.Block
	var diags []Diagnostic

	for _, part := range parts {
		switch p := part.(type) {
		case provider.TextPart:
			subBlocks, subDiags, err := parseTextWithRegistry(r, p.Text, opts)
			if err != nil {
				return nil, nil, err
			}
			baseIndex := len(blocks)
			for _, d := range subDiags {
				d.BlockIndex += baseIndex
				diags = append(diags, d)
			}
			blocks = append(blocks, subBlocks...)

		case provider.ThinkingPart:
			_, ok := r.Lookup("thinking")
			val := thinkingPayload{
				Text:      p.Text,
				Signature: p.Signature,
			}
			if ok {
				raw, _ := json.Marshal(val)
				blocks = append(blocks, domain.Block{
					Type:  "thinking",
					Value: val,
					Raw:   raw,
				})
			} else {
				raw, _ := json.Marshal(val)
				blocks = append(blocks, unknownBlock(raw, blockEnvelope{Type: "thinking"}))
				diags = append(diags, Diagnostic{
					BlockIndex: len(blocks) - 1,
					Code:       "unknown_type",
				})
			}

		case provider.ToolCallPart:
			_, ok := r.Lookup("tool_call")
			val := toolCallPayload{
				CallID:    p.CallID,
				Name:      p.Name,
				Arguments: p.Arguments,
			}
			if ok {
				raw, _ := json.Marshal(val)
				blocks = append(blocks, domain.Block{
					Type:  "tool_call",
					Value: val,
					Raw:   raw,
				})
			} else {
				raw, _ := json.Marshal(val)
				blocks = append(blocks, unknownBlock(raw, blockEnvelope{Type: "tool_call"}))
				diags = append(diags, Diagnostic{
					BlockIndex: len(blocks) - 1,
					Code:       "unknown_type",
				})
			}

		case provider.ImagePart:
			_, ok := r.Lookup("image")
			if ok && p.RemoteID != "" {
				val := imagePayload{
					Ref: p.RemoteID,
				}
				raw, _ := json.Marshal(val)
				blocks = append(blocks, domain.Block{
					Type:  "image",
					Value: val,
					Raw:   raw,
				})
			} else {
				raw, _ := json.Marshal(map[string]any{"remote_id": p.RemoteID})
				blocks = append(blocks, unknownBlock(raw, blockEnvelope{Type: "image"}))
				diags = append(diags, Diagnostic{
					BlockIndex: len(blocks) - 1,
					Code:       "unknown_part",
				})
			}

		case provider.FilePart:
			_, ok := r.Lookup("file")
			if ok && p.RemoteID != "" {
				val := filePayload{
					Ref:  p.RemoteID,
					Name: p.Name,
				}
				raw, _ := json.Marshal(val)
				blocks = append(blocks, domain.Block{
					Type:  "file",
					Value: val,
					Raw:   raw,
				})
			} else {
				raw, _ := json.Marshal(map[string]any{"remote_id": p.RemoteID, "name": p.Name})
				blocks = append(blocks, unknownBlock(raw, blockEnvelope{Type: "file"}))
				diags = append(diags, Diagnostic{
					BlockIndex: len(blocks) - 1,
					Code:       "unknown_part",
				})
			}

		default:
			blocks = append(blocks, domain.Block{
				Type: "unknown",
				Value: map[string]any{
					"raw":          fmt.Sprintf("%v", p),
					"originalType": fmt.Sprintf("%T", p),
				},
			})
			diags = append(diags, Diagnostic{
				BlockIndex: len(blocks) - 1,
				Code:       "unknown_part",
			})
		}
	}

	return blocks, diags, nil
}
