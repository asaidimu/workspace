package content

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/asaidimu/go-anansi/v8/core/common"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
)

// The built-in block types of Workspace specification section 8. Each is
// registered with a default mapper (the wildcard) that every provider
// inherits; a host narrows or replaces one with WithMapper(name, m).

// registerBuiltins adds the ten built-in types to r.
func registerBuiltins(r *registry) error {
	specs := []struct {
		t     BlockType
		maper Mapper
	}{
		{mustType(textType()), mapperFunc(textParts)},
		{mustType(thinkingType()), mapperFunc(thinkingParts)},
		{mustType(imageType()), mapperFunc(imageParts)},
		{mustType(documentType()), mapperFunc(documentParts)},
		{mustType(fileType()), mapperFunc(fileParts)},
		{mustType(jsonType{}, nil), mapperFunc(jsonParts)},
		{mustType(preferenceType()), mapperFunc(preferenceParts)},
		{mustType(toolCallType()), mapperFunc(toolCallParts)},
		{mustType(toolResultType()), mapperFunc(toolResultParts)},
		// summary and tool_digest are workspace bookkeeping: never emittable,
		// never mapped. A summary turn anchors the prompt's fit; tool_digest
		// blocks are spliced into old tool results by the compact stage.
		{mustType(summaryType()), nil},
		{mustType(toolDigestType()), nil},
		// unknown has no mapper on purpose: an unknown block is never sent
		// to a provider.
		{mustType(unknownType()), nil},
	}
	for _, s := range specs {
		var opts []RegisterOption
		if s.maper != nil {
			opts = append(opts, WithMapper(wildcardMapper, s.maper))
		}
		if err := r.Register(s.t, opts...); err != nil {
			return err
		}
	}
	return nil
}

func mustType(t BlockType, err error) BlockType {
	if err != nil {
		panic(err)
	}
	return t
}

// textType is plain or markdown text.
func textType() (BlockType, error) {
	return NewBlockType[textPayload]("text", Consumable|Emittable,
		WithDescription("Plain or markdown text"),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[textPayload](value)
			return p.Text, err
		}),
	)
}

type textPayload struct {
	Text string `json:"text"`
}

func textParts(_ context.Context, b domain.Block, _ provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[textPayload](b.Value)
	if err != nil {
		return nil, err
	}
	return []provider.Part{provider.TextPart{Text: p.Text}}, nil
}

// thinkingType is a reasoning trace: stored for display, not sent back
// unless the provider requires it.
func thinkingType() (BlockType, error) {
	return NewBlockType[thinkingPayload]("thinking", Emittable,
		WithDescription("A reasoning trace, kept for display"),
		WithRules([]string{`{"text": "...", "signature": "..."} (signature optional)`}),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[thinkingPayload](value)
			return p.Text, err
		}),
	)
}

type thinkingPayload struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

func thinkingParts(_ context.Context, b domain.Block, c provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[thinkingPayload](b.Value)
	if err != nil {
		return nil, err
	}
	if !c.Thinking {
		// Not consumable and the provider has no thinking channel: the
		// trace stays in the turn and is not resent.
		return nil, nil
	}
	return []provider.Part{provider.ThinkingPart{Text: p.Text, Signature: p.Signature}}, nil
}

// imageType is a blob reference with a media type.
func imageType() (BlockType, error) {
	return NewBlockType[imagePayload]("image", Consumable,
		WithDescription("A blob reference to an image, with its media type"),
	)
}

type imagePayload struct {
	Ref       string `json:"ref"`
	MediaType string `json:"media_type,omitempty"`
}

func imageParts(ctx context.Context, b domain.Block, _ provider.Capabilities, env MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[imagePayload](b.Value)
	if err != nil {
		return nil, err
	}
	return resolveBlob(ctx, env, p.Ref, "[image omitted]")
}

// documentType is a blob or inline text with a title. It renders as markdown
// ("## title" plus body) — the default compact-JSON renderer would burn
// tokens on braces and key names for the most common content type.
func documentType() (BlockType, error) {
	return NewBlockType[documentPayload]("document", Consumable|Emittable,
		WithDescription("A titled document, inline text or a blob reference"),
		WithRules([]string{
			`inline: {"title": "...", "text": "..."}`,
			`blob: {"title": "...", "ref": "<blob id>", "media_type": "..."}`,
		}),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[documentPayload](value)
			if err != nil {
				return "", err
			}
			if p.Title == "" {
				if p.Text != nil {
					return *p.Text, nil
				}
				return "", nil
			}
			out := "## " + p.Title
			if p.Text != nil && *p.Text != "" {
				out += "\n\n" + *p.Text
			}
			return out, nil
		}),
	)
}

type documentPayload struct {
	Title     string  `json:"title"`
	Text      *string `json:"text,omitempty"`
	Ref       *string `json:"ref,omitempty"`
	MediaType *string `json:"media_type,omitempty"`
}

func documentParts(ctx context.Context, b domain.Block, _ provider.Capabilities, env MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[documentPayload](b.Value)
	if err != nil {
		return nil, err
	}
	if p.Text != nil {
		return []provider.Part{provider.TextPart{Text: *p.Text}}, nil
	}
	return resolveBlob(ctx, env, *p.Ref, "[document omitted]")
}

// fileType is any other blob reference.
func fileType() (BlockType, error) {
	return NewBlockType[filePayload]("file", Consumable,
		WithDescription("A blob reference to any other file"),
	)
}

type filePayload struct {
	Ref       string `json:"ref"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

func fileParts(ctx context.Context, b domain.Block, _ provider.Capabilities, env MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[filePayload](b.Value)
	if err != nil {
		return nil, err
	}
	return resolveBlob(ctx, env, p.Ref, "[file omitted]")
}

// resolveBlob maps a blob reference through the environment. Without a
// resolver (a test prompt, or a host that skipped the port) the block keeps
// its placeholder; with one, resolution is the mapper's job and a failure
// is an error the pipeline records as a block.mapper diagnostic — never a
// silent placeholder.
func resolveBlob(ctx context.Context, env MapEnv, ref, placeholder string) ([]provider.Part, error) {
	if env.Blobs == nil || ref == "" {
		return []provider.Part{provider.TextPart{Text: placeholder}}, nil
	}
	part, err := env.Blobs.Resolve(ctx, env.Owner, ref, "")
	if err != nil {
		return nil, fmt.Errorf("content: resolve blob %q: %w", ref, err)
	}
	return []provider.Part{part}, nil
}

// preferenceType is a named instruction folded into the prompt's
// preferences section.
func preferenceType() (BlockType, error) {
	return NewBlockType[preferencePayload]("preference", Consumable,
		WithDescription("A named instruction, folded into the prompt's preferences section"),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[preferencePayload](value)
			if err != nil {
				return "", err
			}
			return p.Name + ": " + p.Text, nil
		}),
	)
}

type preferencePayload struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

func preferenceParts(_ context.Context, b domain.Block, _ provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[preferencePayload](b.Value)
	if err != nil {
		return nil, err
	}
	return []provider.Part{provider.TextPart{Text: p.Name + ": " + p.Text}}, nil
}

// toolCallType is a request to run a tool.
func toolCallType() (BlockType, error) {
	return NewBlockType[toolCallPayload]("tool_call", Emittable,
		WithDescription("A tool call: name, call ID and arguments"),
		WithRules([]string{`{"call_id": "...", "name": "...", "arguments": {...}}`}),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[toolCallPayload](value)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("[%s] Tool Call: %s(%s)", p.CallID, p.Name, string(p.Arguments)), nil
		}),
	)
}

type toolCallPayload struct {
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func toolCallParts(_ context.Context, b domain.Block, _ provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[toolCallPayload](b.Value)
	if err != nil {
		return nil, err
	}
	return []provider.Part{provider.ToolCallPart{
		CallID:    p.CallID,
		Name:      p.Name,
		Arguments: p.Arguments,
	}}, nil
}

// toolResultType is the output of a tool call.
func toolResultType() (BlockType, error) {
	return NewBlockType[toolResultPayload]("tool_result", Consumable,
		WithDescription("The output of a tool call, with an error flag"),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[toolResultPayload](value)
			if err != nil {
				return "", err
			}
			verb := "Tool Result"
			if p.IsError {
				verb = "Tool Error"
			}
			return fmt.Sprintf("[%s] %s: %s", p.CallID, verb, string(p.Output)), nil
		}),
	)
}

type toolResultPayload struct {
	CallID  string          `json:"call_id"`
	Output  json.RawMessage `json:"output"`
	IsError bool            `json:"is_error,omitempty"`
}

func toolResultParts(_ context.Context, b domain.Block, _ provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
	p, err := bindPayload[toolResultPayload](b.Value)
	if err != nil {
		return nil, err
	}
	return []provider.Part{provider.ToolResultPart{
		CallID:  p.CallID,
		Output:  p.Output,
		IsError: p.IsError,
	}}, nil
}

// unknownType is anything the registry cannot interpret, kept verbatim.
// It is neither consumable nor emittable and has no mapper: an unknown
// block is never sent to a provider.
func unknownType() (BlockType, error) {
	return NewBlockType[unknownPayload]("unknown", 0,
		WithDescription("An uninterpretable block, kept verbatim"),
	)
}

type unknownPayload struct {
	Raw          json.RawMessage `json:"raw"`
	OriginalType string          `json:"originalType,omitempty"`
}

// jsonType is structured data: any object, free-form. It is hand-written
// rather than derived because its payload is open — there is no DTO to
// reflect.
type jsonType struct{}

func (jsonType) Name() string             { return "json" }
func (jsonType) Capabilities() Capability { return Consumable | Emittable }
func (jsonType) Predicates() definition.PredicateMap {
	return nil
}
func (jsonType) Describe() string { return "Structured data, validated by a schema at the call site" }
func (jsonType) Rules() []string {
	return []string{`the payload is any JSON object, e.g. {"items": [1, 2, 3]}`}
}
func (jsonType) Schema() *definition.Schema {
	s, err := SchemaFor(reflect.TypeFor[struct{}]())
	if err != nil {
		// An empty struct always reflects; failure would be a broken build.
		panic("content: json schema: " + err.Error())
	}
	return s
}

// Decode accepts any object and keeps it as the typed value.
func (jsonType) Decode(payload map[string]any) (any, []common.Issue) {
	if payload == nil {
		return nil, []common.Issue{{Code: "invalid", Message: "the json block payload must be an object", Severity: "error"}}
	}
	return payload, nil
}

func (jsonType) Encode(value any) (map[string]any, error) {
	if m, ok := value.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return decodeObject(b)
}

func (t jsonType) Render(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("content: render json: %w", err)
	}
	return string(b), nil
}

func jsonParts(_ context.Context, b domain.Block, _ provider.Capabilities, _ MapEnv) ([]provider.Part, error) {
	raw := b.Raw
	if len(raw) == 0 && b.Value != nil {
		var err error
		raw, err = json.Marshal(b.Value)
		if err != nil {
			return nil, fmt.Errorf("content: map json block: %w", err)
		}
	}
	return []provider.Part{provider.TextPart{Text: string(raw)}}, nil
}

// summaryType is a committed history digest: the workspace's record that the
// turns it covers are represented by its text. Never emittable — the model
// never invents summaries; the background compactor commits them as system
// turns, and the prompt pipeline anchors the fit at the newest one.
func summaryType() (BlockType, error) {
	return NewBlockType[summaryPayload]("summary", Consumable,
		WithDescription("A committed history digest produced by the workspace"),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[summaryPayload](value)
			return p.Text, err
		}),
	)
}

type summaryPayload struct {
	Text   string   `json:"text"`
	Covers []string `json:"covers,omitempty"`
	Origin string   `json:"origin,omitempty"`
}

// toolDigestType is one committed narrative line for one old tool round,
// keyed by call ID. Never emittable and never mapped: the compact stage
// splices the text into the tool_result body it digests.
func toolDigestType() (BlockType, error) {
	return NewBlockType[toolDigestPayload]("tool_digest", Consumable,
		WithDescription("A committed one-line digest of a completed tool round"),
		WithRender(func(value any) (string, error) {
			p, err := bindPayload[toolDigestPayload](value)
			return p.Text, err
		}),
	)
}

type toolDigestPayload struct {
	CallID string `json:"call_id"`
	Text   string `json:"text"`
}

// bindPayload converts a block's value — a typed payload or a plain
// map, whichever the producer left there — into T.
func bindPayload[T any](value any) (T, error) {
	var out T
	if value == nil {
		return out, fmt.Errorf("content: missing payload")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return out, fmt.Errorf("content: bind payload: %w", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("content: bind payload: %w", err)
	}
	return out, nil
}

// mapperFunc adapts a function to the Mapper interface.
type mapperFunc func(ctx context.Context, b domain.Block, c provider.Capabilities, env MapEnv) ([]provider.Part, error)

func (f mapperFunc) ToParts(ctx context.Context, b domain.Block, c provider.Capabilities, env MapEnv) ([]provider.Part, error) {
	return f(ctx, b, c, env)
}
