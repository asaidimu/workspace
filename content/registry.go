package content

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/asaidimu/go-anansi/v8/core/common"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
)

// typeConfig accumulates the optional settings for one block type.
type typeConfig struct {
	predicates definition.PredicateMap
	desc       string
	rules      []string
	render     func(value any) (string, error)
}

// TypeOption configures a block type at construction.
type TypeOption func(*typeConfig)

// WithPredicates attaches cross-field and semantic rules that a schema
// cannot express, for example "this blob exists and belongs to the owner".
func WithPredicates(p definition.PredicateMap) TypeOption {
	return func(c *typeConfig) { c.predicates = p }
}

// WithDescription sets the type's human-readable description.
func WithDescription(text string) TypeOption {
	return func(c *typeConfig) { c.desc = text }
}

// WithRules attaches usage rules for the block type in prompt instructions.
func WithRules(rules []string) TypeOption {
	return func(c *typeConfig) { c.rules = append([]string(nil), rules...) }
}

// WithRender replaces the default value-to-string renderer.
func WithRender(fn func(value any) (string, error)) TypeOption {
	return func(c *typeConfig) { c.render = fn }
}

// NewBlockType derives the schema from T's anansi tags, compiles the
// validator once, and returns a BlockType whose typed value is a T.
func NewBlockType[T any](name string, caps Capability, opts ...TypeOption) (BlockType, error) {
	if name == "" {
		return nil, fmt.Errorf("content: block type needs a name")
	}
	cfg := &typeConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	schema, err := SchemaFor(reflect.TypeFor[T]())
	if err != nil {
		return nil, fmt.Errorf("content: block type %s: %w", name, err)
	}
	validator, err := definition.NewDocumentValidator(schema, cfg.predicates)
	if err != nil {
		return nil, fmt.Errorf("content: block type %s: %w", name, err)
	}
	return &blockType[T]{
		name:       name,
		caps:       caps,
		schema:     schema,
		validator:  validator,
		predicates: cfg.predicates,
		desc:       cfg.desc,
		rules:      cfg.rules,
		render:     cfg.render,
	}, nil
}

// blockType is the generic BlockType: one schema, one compiled validator
// and one typed value per registration.
type blockType[T any] struct {
	name       string
	caps       Capability
	schema     *definition.Schema
	validator  *definition.DocumentValidator
	predicates definition.PredicateMap
	desc       string
	rules      []string
	render     func(value any) (string, error)
}

func (t *blockType[T]) Name() string                        { return t.name }
func (t *blockType[T]) Capabilities() Capability            { return t.caps }
func (t *blockType[T]) Schema() *definition.Schema          { return t.schema }
func (t *blockType[T]) Predicates() definition.PredicateMap { return t.predicates }
func (t *blockType[T]) Describe() string                    { return t.desc }
func (t *blockType[T]) Rules() []string                     { return t.rules }

// Decode validates the payload strictly against the compiled schema, then
// binds it to T through a JSON round-trip — the stand-in for anansi's
// binder, which lives in core/data and is out of reach from here.
func (t *blockType[T]) Decode(payload map[string]any) (any, []common.Issue) {
	if payload == nil {
		payload = map[string]any{}
	}
	issues, ok := t.validator.Validate(payload)
	if !ok {
		return nil, issues
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, []common.Issue{{Code: "invalid", Message: err.Error(), Severity: "error"}}
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, []common.Issue{{Code: "invalid", Message: err.Error(), Severity: "error"}}
	}
	return v, issues
}

// Encode turns a typed value back into its payload map.
func (t *blockType[T]) Encode(value any) (map[string]any, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("content: encode %s: %w", t.name, err)
	}
	return decodeObject(b)
}

// Render is the default renderer: the payload as compact JSON. Types that
// want prose override it with WithRender.
func (t *blockType[T]) Render(value any) (string, error) {
	if t.render != nil {
		return t.render(value)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("content: render %s: %w", t.name, err)
	}
	return string(b), nil
}

// wildcardMapper is the registration key for a type's default mapper, used
// when no provider-specific mapper was registered.
const wildcardMapper = "*"

// registry is the concrete block-type table.
type registry struct {
	mu    sync.RWMutex
	types map[string]BlockType
	order []string
	regs  map[string]registration
}

// NewRegistry returns a registry with the built-in types of section 8
// already registered, each with its default mapper.
func NewRegistry() Registry {
	r := &registry{
		types: map[string]BlockType{},
		regs:  map[string]registration{},
	}
	if err := registerBuiltins(r); err != nil {
		// The built-ins are compiled from types in this package; a failure
		// here is a programmer mistake, not a runtime condition.
		panic("content: built-in block types: " + err.Error())
	}
	return r
}

// newEmptyRegistry is the bare table, for tests and for hosts that want to
// register their own set from scratch.
func newEmptyRegistry() *registry {
	return &registry{
		types: map[string]BlockType{},
		regs:  map[string]registration{},
	}
}

func (r *registry) Register(t BlockType, opts ...RegisterOption) error {
	if t == nil || t.Name() == "" {
		return fmt.Errorf("content: cannot register a nil or unnamed block type")
	}
	reg := registration{mappers: map[string]Mapper{}}
	for _, opt := range opts {
		opt(&reg)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.types[t.Name()]; dup {
		return fmt.Errorf("content: block type %q is already registered", t.Name())
	}
	r.types[t.Name()] = t
	r.order = append(r.order, t.Name())
	r.regs[t.Name()] = reg
	return nil
}

func (r *registry) Lookup(name string) (BlockType, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.types[name]
	return t, ok
}

// Types returns every registered type, sorted by name for determinism.
func (r *registry) Types() []BlockType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := append([]string(nil), r.order...)
	sort.Strings(names)
	out := make([]BlockType, 0, len(names))
	for _, n := range names {
		out = append(out, r.types[n])
	}
	return out
}

// MapperFor resolves the mapper for one type and provider. A
// provider-specific mapper wins; otherwise the type's default mapper (the
// wildcard) is used.
func (r *registry) MapperFor(typeName, providerName string) (Mapper, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.regs[typeName]
	if !ok {
		return nil, false
	}
	if m, ok := reg.mappers[providerName]; ok {
		return m, true
	}
	m, ok := reg.mappers[wildcardMapper]
	return m, ok
}

// EffectsFor resolves the post-commit effects for a type, if any.
func (r *registry) EffectsFor(typeName string) (Effects, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.regs[typeName]
	if !ok || reg.effects == nil {
		return nil, false
	}
	return reg.effects, true
}

// blockEnvelope is the wire shape of one element of Parse's input: the
// domain.Block JSON (Type/Key/Value, matched case-insensitively) or any
// object carrying at least a type name.
type blockEnvelope struct {
	Type  string          `json:"Type"`
	Key   string          `json:"Key"`
	Value json.RawMessage `json:"Value"`
}

// Parse decodes a JSON array of block objects. A block whose type is not
// registered, or whose payload fails validation, is kept verbatim as an
// "unknown" block with a diagnostic — never dropped, never stored as typed.
func (r *registry) Parse(raw json.RawMessage) ([]domain.Block, []Diagnostic, error) {
	var elements []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&elements); err != nil {
		return nil, nil, fmt.Errorf("content: parse blocks: %w", err)
	}
	blocks := make([]domain.Block, 0, len(elements))
	diags := make([]Diagnostic, 0)
	for i, element := range elements {
		var env blockEnvelope
		if err := json.Unmarshal(element, &env); err != nil {
			return nil, nil, fmt.Errorf("content: parse block %d: %w", i, err)
		}
		t, ok := r.Lookup(env.Type)
		if !ok {
			blocks = append(blocks, unknownBlock(element, env))
			diags = append(diags, Diagnostic{BlockIndex: i, Code: "unknown_type"})
			continue
		}
		payload, err := decodeObject(env.Value)
		if err != nil {
			blocks = append(blocks, unknownBlock(element, env))
			diags = append(diags, Diagnostic{
				BlockIndex: i,
				Code:       "invalid",
				Issues:     []common.Issue{{Code: "invalid", Message: err.Error(), Severity: "error"}},
			})
			continue
		}
		value, issues := t.Decode(payload)
		if value == nil {
			blocks = append(blocks, unknownBlock(element, env))
			diags = append(diags, Diagnostic{BlockIndex: i, Code: "invalid", Issues: issues})
			continue
		}
		blocks = append(blocks, domain.Block{
			Type:  env.Type,
			Key:   env.Key,
			Value: value,
			Raw:   append(json.RawMessage(nil), env.Value...),
		})
	}
	return blocks, diags, nil
}

// unknownBlock preserves an uninterpretable block verbatim: the original
// bytes stay in Raw, and the value keeps both the raw object and the type
// name the registry could not resolve (or could not validate).
func unknownBlock(element json.RawMessage, env blockEnvelope) domain.Block {
	raw, _ := decodeObject(element)
	if raw == nil {
		raw = map[string]any{}
	}
	return domain.Block{
		Type: "unknown",
		Key:  env.Key,
		Value: map[string]any{
			"raw":          raw,
			"originalType": env.Type,
		},
		Raw: append(json.RawMessage(nil), element...),
	}
}

// decodeObject decodes JSON bytes into an object with number fidelity.
func decodeObject(b []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// registration is the per-type optional wiring passed at Register time.
type registration struct {
	mappers map[string]Mapper
	effects Effects
}

// RegisterOption attaches a provider mapper or effects to one registration.
type RegisterOption func(*registration)

// WithMapper attaches the mapper for one provider name.
func WithMapper(providerName string, m Mapper) RegisterOption {
	return func(reg *registration) {
		if reg.mappers == nil {
			reg.mappers = map[string]Mapper{}
		}
		reg.mappers[providerName] = m
	}
}

// WithEffects attaches the post-commit effects for the type.
func WithEffects(e Effects) RegisterOption {
	return func(reg *registration) { reg.effects = e }
}

// Effects is what a committed block does next: it may yield further commands,
// dispatched under the turn's principal with an idempotency key of the turn
// reference plus the block index, so a replay changes nothing.
type Effects interface {
	Effects(ctx context.Context, b domain.Block) ([]kernel.Command, error)
}

// Mapper converts a block into provider parts, downgrading when the provider
// lacks a capability. It never fails silently.
type Mapper interface {
	ToParts(ctx context.Context, b domain.Block, c provider.Capabilities, env MapEnv) ([]provider.Part, error)
}

// MapEnv is the environment a mapper may consult — see blob.go for the
// contract. Alias here so Mapper's signature names it from the registry.

// Diagnostic reports a block that could not be interpreted, could not be
// validated, or had to be downgraded. Code is "unknown_type", "invalid" or
// "downgraded".
type Diagnostic struct {
	BlockIndex int
	Code       string
	Issues     []common.Issue
}

// Registry is the block-type table.
type Registry interface {
	Register(t BlockType, opts ...RegisterOption) error
	Lookup(name string) (BlockType, bool)
	Types() []BlockType
	MapperFor(typeName, providerName string) (Mapper, bool)
	EffectsFor(typeName string) (Effects, bool)
	Parse(raw json.RawMessage) ([]domain.Block, []Diagnostic, error)
	ParseText(text string) ([]domain.Block, []Diagnostic, error)
}
