// Package prompt turns a conversation snapshot into a provider request in
// six deterministic stages, so the same inputs always give a byte-identical
// prompt. That property makes golden tests possible and makes provider
// switching safe.
//
//  1. Snapshot — read the conversation, role, settings, attached content and
//     the active chain in one read transaction.
//  2. Select — gather candidate content: attachments, role and workspace
//     preferences, and items from the retriever.
//  3. Resolve — pick versions, drop tombstoned keys, apply preference
//     precedence and record conflicts as diagnostics.
//  4. Compact — fit the token budget, anchored at the newest committed
//     summary turn and splicing committed tool digests. The build itself
//     never calls a model; summarization happens in the background (see
//     compaction.go) and only committed state is ever read back.
//  5. Assemble — build the ordered system sections and the transcript.
//  6. Map — convert blocks to provider parts, apply capability downgrades,
//     and attach tool specs and an output schema when needed.
package prompt

import (
	"context"
	"strings"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// Stage is one step of the pipeline. Stages run in the order they were
// supplied to NewPipeline.
type Stage interface {
	Name() string
	Run(ctx context.Context, b *Build) error
}

// Build is the accumulating state the stages fill in.
type Build struct {
	Snapshot    Snapshot
	Candidates  []Candidate
	Sections    []Section
	Transcript  []domain.Turn
	Budget      Budget
	Request     provider.Request
	Diagnostics []Diagnostic

	// input carries the caller's Input to the stages, which can only see a
	// *Build. The exported shape stays exactly as section 9 declares it.
	input Input

	// sectionCache memoizes sectionBodies so the compact and assemble stages
	// agree on one deterministic result without rebuilding it.
	sectionCache []Section
	// resolved is the precedence-ordered content resolve produced.
	resolved []resolvedItem
	// tokenizer is the counting strategy; nil selects Estimator.
	tokenizer Tokenizer
}

// Section is one system prompt section. Sections sort by Order then ID, so
// assembly is deterministic and nothing uses the clock or randomness.
type Section struct {
	ID    string
	Title string
	Body  string
	Order int
}

// Content renders the section the TS assembler's way: a markdown header
// line, then the body. Sections join with SystemSeparator into one system
// part.
func (s Section) Content() string {
	if strings.TrimSpace(s.Title) == "" {
		return s.Body
	}
	return "# " + s.Title + "\n" + s.Body
}

// SystemSeparator joins system sections (the TS assembler's
// SECTION_SEPARATOR).
const SystemSeparator = "\n\n---\n\n"

// Budget is the token budget for one build.
type Budget struct {
	ContextWindow  int
	ReservedOutput int
	Used           int
}

// RetrievalQuery asks the retriever for content relevant to a conversation.
type RetrievalQuery struct {
	Owner          string
	ConversationID string
	Topics         []string
	Text           string
	Limit          int
}

// Scored is a retrieval hit with its rank.
type Scored struct {
	Ref   store.ContentRef
	Score float64
}

// Retriever ranks candidate content. The default ranks by topic overlap with
// the conversation, then by recency, with a configurable limit (default 20).
// Full-text and embedding retrievers plug in behind the same interface.
type Retriever interface {
	Retrieve(ctx context.Context, q RetrievalQuery) ([]Scored, error)
}

// Tokenizer counts the tokens a model will charge for a request.
type Tokenizer interface {
	Count(model string, parts []provider.Part) (int, error)
}

// Pipeline is the ordered stage list.
type Pipeline struct {
	stages []Stage
}

// NewPipeline wires the stages. The caller owns the order.
func NewPipeline(stages ...Stage) *Pipeline { return &Pipeline{stages: stages} }

// DefaultPipeline is the six stages from section 9 in order. A host that
// wants a different order, or fewer stages, wires them with NewPipeline.
func DefaultPipeline() *Pipeline {
	return NewPipeline(
		snapshotStage{},
		selectStage{},
		resolveStage{},
		compactStage{},
		assembleStage{},
		mapStage{},
	)
}

// Build runs every stage and returns the provider request plus any
// diagnostics the stages recorded. It seeds the Build from the Input, so a
// custom stage list sees the same starting state the default stages do.
func (p *Pipeline) Build(ctx context.Context, in Input) (provider.Request, []Diagnostic, error) {
	b, err := p.run(ctx, in)
	if b == nil {
		return provider.Request{}, nil, err
	}
	return b.Request, b.Diagnostics, err
}

// Prepare runs every stage and returns the full prepared result: the
// request, the assembled sections, the token accounting and the output
// schema. Build is the narrow form of it.
func (p *Pipeline) Prepare(ctx context.Context, in Input) (Result, error) {
	b, err := p.run(ctx, in)
	if b == nil {
		return Result{}, err
	}
	res := Result{
		Request:     b.Request,
		Sections:    b.Sections,
		Diagnostics: b.Diagnostics,
		Schema:      b.Request.OutputSchema,
		tokenizer:   b.tokenizer,
		model:       in.Model,
		Budget:      b.Budget,
	}
	return res, nil
}

// run validates the Input, seeds a Build and executes the stages. It
// returns the Build even on a stage error so the caller can see the
// diagnostics collected before the failure.
func (p *Pipeline) run(ctx context.Context, in Input) (*Build, error) {
	if err := ctx.Err(); err != nil {
		return nil, kernel.WrapError(kernel.CodeCanceled, "build canceled", err)
	}
	if in.Snapshot.Conversation.ID == "" {
		return nil, kernel.NewError(kernel.CodeInvalid, "the snapshot has no conversation")
	}
	// Extension labels name sections, so they must be unique (registration
	// rule: a duplicate registration is an error).
	seen := make(map[string]bool, len(in.Extensions))
	for _, e := range in.Extensions {
		if e.Label == "" {
			return nil, kernel.NewError(kernel.CodeInvalid, "an extension has no label")
		}
		if seen[e.Label] {
			return nil, kernel.NewError(kernel.CodeInvalid, "duplicate extension label "+e.Label)
		}
		seen[e.Label] = true
	}

	b := &Build{
		Snapshot:   in.Snapshot,
		Transcript: in.Snapshot.Transcript,
		input:      in,
	}
	b.Budget = Budget{ContextWindow: in.ContextWindow}
	// Reserved output is the request's maximum output tokens, or 10% of the
	// window when unset (section 9, budget rules).
	b.Budget.ReservedOutput = in.MaxOutputTokens
	if b.Budget.ReservedOutput <= 0 && in.ContextWindow > 0 {
		b.Budget.ReservedOutput = in.ContextWindow / 10
	}
	if in.Tokenizer != nil {
		b.tokenizer = in.Tokenizer
	} else {
		b.tokenizer = Estimator{}
		b.diagnostics("token.approximate", "token counts are estimated as bytes/4 and are approximate", nil)
	}

	for _, st := range p.stages {
		if err := st.Run(ctx, b); err != nil {
			return b, err
		}
	}
	return b, nil
}

// diagnostics records a non-fatal problem, which the caller sees alongside
// the request. Callers branch on Code.
func (b *Build) diagnostics(code, message string, ref *store.ContentRef) {
	b.Diagnostics = append(b.Diagnostics, Diagnostic{Code: code, Message: message, Ref: ref})
}
