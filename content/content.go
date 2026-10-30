// Package content is the block registry: it decides what each block type
// means — how it validates, how the model sees it, which provider parts it
// maps to, and what happens after it is committed.
//
// Turns and content both hold blocks, so one registry serves both. A block
// type declares two capabilities: Consumable (may appear in prompts) and
// Emittable (the model may produce it). Content access modes map onto these:
// read is consumable, write is emittable, readwrite is both, internal is
// neither.
//
// definition and common are anansi's schema and error packages, which this
// package is allowed to import (Workspace specification, section 5).
package content

import (
	"github.com/asaidimu/go-anansi/v8/core/common"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
)

// Capability is a bit set of what a block type may do.
type Capability uint8

const (
	Consumable Capability = 1 << iota
	Emittable
)

// BlockType is the registry's contract for one block type.
type BlockType interface {
	Name() string
	Capabilities() Capability
	Schema() *definition.Schema          // the payload DTO as an anansi schema
	Predicates() definition.PredicateMap // cross-field and semantic rules, may be nil
	Describe() string
	Rules() []string
	Decode(payload map[string]any) (any, []common.Issue) // validates strictly, then binds to the typed value
	Encode(value any) (map[string]any, error)
	Render(value any) (string, error)
}
