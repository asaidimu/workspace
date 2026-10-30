package prompt

import (
	"encoding/json"

	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// Snapshot is the consistent read of everything one build needs, taken in a
// single read transaction so the prompt cannot straddle a commit.
type Snapshot struct {
	Conversation domain.Conversation
	Role         *domain.Role
	Settings     domain.Settings
	Attached     []domain.Content
	Transcript   []domain.Turn
	Topics       []string
}

// Extension is an extra system section the caller anchors into the section
// list (section 9: "Extensions add sections with an anchor: before or after
// a named section, or prepend or append to the list"). Label names the
// section, Position is "prepend", "append", "before:<section-id>" or
// "after:<section-id>", where the id is one of the built-in section IDs
// (policy, tools, blocks, persona, preferences, context, artifacts,
// instructions); an anchor that names a section the build did not assemble
// records a diagnostic and the extension is appended instead.
type Extension struct {
	Label    string
	Content  string
	Position string
}

// Candidate is one piece of content the build may include, after selection
// and before resolution.
type Candidate struct {
	Ref   store.ContentRef
	Kind  CandidateKind
	Score float64
	Topic string
}

// CandidateKind says where a candidate came from, which decides which system
// section it lands in.
type CandidateKind int

const (
	// FromConversationPreference is a preference key listed on the conversation.
	FromConversationPreference CandidateKind = iota
	// FromRolePreference is a preference key listed on the role.
	FromRolePreference
	// FromWorkspacePreference is a global preference key.
	FromWorkspacePreference
	// FromAttachment is content linked to the conversation itself.
	FromAttachment
	// FromRetriever is a hit from the configured retriever.
	FromRetriever
)

// Summary is a compactor's replacement for a run of elided turns.
type Summary struct {
	Text        string
	TurnRefs    []domain.TurnRef
	InputTokens int
}

// Input is what the caller hands Pipeline.Build.
type Input struct {
	Snapshot     Snapshot
	Instructions string
	Model        string

	// Content is the block registry: drives block rendering, emittable block types,
	// and transcript part mapping.
	Content content.Registry

	// ProviderName identifies the provider adapter (e.g. "openai", "anthropic", "gemini").
	// It selects provider-specific block mappers in the registry.
	ProviderName string

	// Capabilities are the model's capabilities; mappers use them to
	// downgrade blocks the provider cannot take (vision, thinking, PDF).
	Capabilities provider.Capabilities

	// NativeTools indicates whether native tool-calling is supported/enabled.
	NativeTools bool

	// Constraints holds the caller's per-model constraints (the last user
	// turn's opinion). They win over the conversation's and the role's; see
	// applyConstraints in the map stage for the precedence order.
	Constraints *domain.Constraints

	// Blobs is the blob resolver used to resolve media references for the provider.
	Blobs content.BlobResolver

	// Tools are the specs to expose. In native mode (the default) they are
	// attached to the request; in structured mode they are described in a
	// system section instead, per section 9's table.
	Tools []provider.ToolSpec

	// ContextWindow is the model's context window in tokens, and
	// MaxOutputTokens is the request's output budget. Reserved output is
	// MaxOutputTokens, or 10% of the window when it is unset.
	ContextWindow   int
	MaxOutputTokens int

	// Tokenizer counts tokens; nil selects the byte/4 estimator, which marks
	// every count approximate in a diagnostic.
	Tokenizer Tokenizer

	// Retriever and Compactor are optional; nil means no retrieval and no
	// summarization of elided turns.
	Retriever Retriever
	Compactor Compactor

	// Policy is the fixed operating rules from the workspace. It is the one
	// section that is always present.
	Policy string

	// Structured selects structured-output mode, which the first release
	// defers (decision D5). Native tool calling is the default.
	Structured bool

	// OutputSchema is attached only in structured mode.
	OutputSchema json.RawMessage

	// RetrievalLimit bounds retriever hits; zero selects 20.
	RetrievalLimit int

	// Extensions are extra sections anchored to the built-ins. Labels must
	// be unique within one build; assembly is still sorted by Order then ID.
	Extensions []Extension
}

// Diagnostic records a non-fatal problem found while building: a dropped
// tombstoned key, a preference conflict, an approximate token count.
type Diagnostic struct {
	Code    string
	Message string
	Ref     *store.ContentRef
}

// Result is the detailed preparation outcome returned by Pipeline.Prepare.
type Result struct {
	Request     provider.Request
	Sections    []Section
	Budget      Budget
	Schema      json.RawMessage
	Diagnostics []Diagnostic

	// tokenizer and model back the Tokens method; they are not part of the
	// public result shape.
	tokenizer Tokenizer
	model     string
}

// Tokens counts each assembled section once, deterministically, and reports
// the window accounting the build used.
func (r Result) Tokens() ResultTokens {
	t := ResultTokens{
		ContextWindow:  r.Budget.ContextWindow,
		ReservedOutput: r.Budget.ReservedOutput,
		Total:          r.Budget.Used,
		Remaining:      r.Budget.ContextWindow - r.Budget.ReservedOutput - r.Budget.Used,
		Sections:       make(map[string]int, len(r.Sections)),
	}
	var tok Tokenizer = Estimator{}
	if r.tokenizer != nil {
		tok = r.tokenizer
	}
	for _, s := range r.Sections {
		n, err := tok.Count(r.model, []provider.Part{provider.TextPart{Text: s.Content()}})
		if err != nil {
			continue
		}
		t.Sections[s.ID] = n
	}
	return t
}

// ResultTokens contains token accounting details across the request.
type ResultTokens struct {
	// ContextWindow and ReservedOutput mirror the Budget the build used.
	ContextWindow  int
	ReservedOutput int
	// Total is the tokens the build charged (fixed sections plus the
	// transcript it kept).
	Total int
	// Remaining is what is left of the window after reserved output.
	Remaining int
	// Sections is the per-section token count, keyed by section ID.
	Sections map[string]int
}
