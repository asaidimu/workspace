package agent

import (
	"slices"
	"sort"
	"sync"

	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
)

// table is the in-memory Registry. It is a plain map guarded by a RWMutex:
// tools are registered at startup and looked up on every step, so the
// common path is a read.
type table struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty in-memory tool table.
func NewRegistry() Registry {
	return &table{tools: map[string]Tool{}}
}

func (r *table) Register(t Tool) error {
	if t == nil {
		return kernel.NewError(kernel.CodeInvalid, "a nil tool cannot be registered")
	}
	spec := t.Spec()
	if spec.Name == "" {
		return kernel.NewError(kernel.CodeInvalid, "a tool must have a name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.tools[spec.Name]; taken {
		return kernel.NewError(kernel.CodeConflict, "tool already registered")
	}
	r.tools[spec.Name] = t
	return nil
}

func (r *table) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Specs returns the tools p may call, sorted by name so the request the
// pipeline builds is byte-stable. A tool is visible only when p holds every
// scope the tool asks for, so a tool can narrow who reaches it and can never
// widen the principal: a principal with no scopes can call only the tools
// that declare none.
func (r *table) Specs(p kernel.Principal) []provider.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]provider.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		spec := t.Spec()
		if !hasAllScopes(p, spec.Scopes) {
			continue
		}
		out = append(out, provider.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Schema:      spec.Schema,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// hasAllScopes reports whether p holds every scope the tool declares. An
// empty requirement is always satisfied.
func hasAllScopes(p kernel.Principal, required []string) bool {
	for _, scope := range required {
		if !slices.Contains(p.Scopes, scope) {
			return false
		}
	}
	return true
}
