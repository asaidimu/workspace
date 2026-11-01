package prompt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
)

// Section orders. Persona leads the system prompt (identity primes tone for
// everything after it), policy follows as the always-present operating rules.
// Extensions anchor before or after a named section, or prepend or append to
// the list; the numbers here are the anchors they anchor to.
const (
	// The structured-output preamble sits before every built-in section
	// (order -1), so a prepended extension (0) still lands between the
	// preamble and the persona section.
	orderOperatingSystem = -1
	orderPersona         = 1
	orderPolicy          = 2
	orderTools           = 3
	orderBlocks          = 4
	orderPreferences     = 6
	orderContext         = 7
	orderArtifacts       = 8
	orderInstructions    = 9

	// Extensions resolve around the built-ins: prepend lands before persona,
	// and append — together with any anchor whose section was not assembled —
	// lands after instructions. Order 5 is deliberately left free so
	// after:persona (2) and after:policy (3) have room to breathe before
	// the preference section.
	orderExtensionFirst = 0
	orderExtensionLast  = 10
)

// resolvedItem is content that survived selection and resolution, paired with
// where it came from, because the source decides which section it lands in.
type resolvedItem struct {
	Content domain.Content
	Kind    CandidateKind
}

// --- stage 1: snapshot ---

type snapshotStage struct{}

func (snapshotStage) Name() string { return "snapshot" }

// Run adopts the caller's snapshot. The read itself happened in one read
// transaction before the build, so the prompt cannot straddle a commit; this
// stage only seeds the transcript the later stages trim.
func (snapshotStage) Run(ctx context.Context, b *Build) error {
	b.Transcript = b.Snapshot.Transcript
	return nil
}

// --- stage 2: select ---

type selectStage struct{}

func (selectStage) Name() string { return "select" }

// Run gathers candidate content: attachments, role and workspace
// preferences, and items from the retriever.
func (selectStage) Run(ctx context.Context, b *Build) error {
	for _, c := range b.Snapshot.Attached {
		b.Candidates = append(b.Candidates, Candidate{Ref: c.Ref(), Kind: FromAttachment})
	}
	for _, ref := range b.Snapshot.Conversation.PreferenceKeys {
		b.Candidates = append(b.Candidates, Candidate{Ref: ref, Kind: FromConversationPreference})
	}
	if b.Snapshot.Role != nil {
		for _, ref := range b.Snapshot.Role.PreferenceKeys {
			b.Candidates = append(b.Candidates, Candidate{Ref: ref, Kind: FromRolePreference})
		}
	}
	for _, ref := range b.Snapshot.Settings.GlobalPreferenceKeys {
		b.Candidates = append(b.Candidates, Candidate{Ref: ref, Kind: FromWorkspacePreference})
	}
	if b.input.Retriever == nil {
		return nil
	}

	q := RetrievalQuery{
		Owner:          b.Snapshot.Conversation.Owner,
		ConversationID: b.Snapshot.Conversation.ID,
		Topics:         b.Snapshot.Topics,
		Text:           transcriptText(b.Transcript),
		Limit:          b.input.RetrievalLimit,
	}
	if q.Limit <= 0 {
		q.Limit = 20 // section 9's default limit
	}
	hits, err := b.input.Retriever.Retrieve(ctx, q)
	if err != nil {
		return kernel.WrapError(kernel.CodeUnavailable, "retrieval failed", err)
	}
	for _, h := range hits {
		b.Candidates = append(b.Candidates, Candidate{Ref: h.Ref, Kind: FromRetriever, Score: h.Score})
	}
	return nil
}

// transcriptText flattens the turn text a retriever ranks against.
func transcriptText(turns []domain.Turn) string {
	var sb strings.Builder
	for _, t := range turns {
		for _, blk := range t.Blocks {
			if txt, ok := blockText(blk); ok {
				sb.WriteString(txt)
				sb.WriteByte(' ')
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// --- stage 3: resolve ---

type resolveStage struct{}

func (resolveStage) Name() string { return "resolve" }

// Run picks versions, drops tombstoned keys, applies preference precedence
// and records conflicts as diagnostics.
func (resolveStage) Run(ctx context.Context, b *Build) error {
	// The snapshot carries the content values; a candidate only names a ref.
	byKey := map[string]domain.Content{}
	for _, c := range b.Snapshot.Attached {
		if prev, ok := byKey[c.Key]; !ok || c.Version > prev.Version {
			byKey[c.Key] = c
		}
	}

	// One entry per key, keeping the highest-precedence source. Offers are
	// tracked separately so a key offered by more than one distinct source
	// is reported even when the winner is unambiguous — the caller should
	// know the choice was made.
	best := map[string]Candidate{}
	offers := map[string]map[offer]bool{}
	for _, cand := range b.Candidates {
		key := cand.Ref.Key
		set := offers[key]
		if set == nil {
			set = map[offer]bool{}
			offers[key] = set
		}
		set[offer{kind: cand.Kind, version: cand.Ref.Version, score: cand.Score}] = true

		cur, ok := best[key]
		if !ok || betterCandidate(cand, cur) {
			best[key] = cand
		}
	}

	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if len(offers[key]) > 1 {
			ref := best[key].Ref
			b.diagnostics("content.conflict", "multiple sources offered "+key, &ref)
		}

		cand := best[key]
		content, ok := byKey[key]
		if !ok {
			ref := cand.Ref
			b.diagnostics("content.unresolved", "no content for "+key, &ref)
			continue
		}
		if content.Tombstoned {
			ref := content.Ref()
			b.diagnostics("content.tombstoned", "dropped tombstoned "+key, &ref)
			continue
		}
		b.resolved = append(b.resolved, resolvedItem{Content: content, Kind: cand.Kind})
	}
	return nil
}

// offer is one distinct claim on a key, used to detect conflicts.
type offer struct {
	kind    CandidateKind
	version int
	score   float64
}

// betterCandidate reports whether a outranks b. Version choice and section
// assignment are the same decision here: a key that is both attached and
// listed as a preference is a preference, because that is its purpose, so
// specificity ranks preference above attachment above retrieval. A higher
// score wins inside the retriever tier.
func betterCandidate(a, b Candidate) bool {
	sa, sb := specificity(a.Kind), specificity(b.Kind)
	if sa != sb {
		return sa < sb
	}
	if a.Kind == FromRetriever && a.Score != b.Score {
		return a.Score > b.Score
	}
	// Same source tier: the higher version wins, which is the "pick versions"
	// half of resolve.
	if a.Ref.Version != b.Ref.Version {
		return a.Ref.Version > b.Ref.Version
	}
	return false
}

// specificity orders candidate sources from most to least intentional. It is
// deliberately not the CandidateKind order: an attachment is a weaker claim
// on a key than a preference is. Precedence is conversation > role > workspace.
func specificity(k CandidateKind) int {
	switch k {
	case FromConversationPreference:
		return 0
	case FromRolePreference:
		return 1
	case FromWorkspacePreference:
		return 2
	case FromAttachment:
		return 3
	default:
		return 4
	}
}

// --- stage 4: compact ---

type compactStage struct{}

func (compactStage) Name() string { return "compact" }

// Run fits the token budget. Fixed sections are costed first: if they alone
// exceed the budget the build fails, because that is a configuration problem
// no amount of transcript omission can fix.
//
// The build itself never calls a model. Committed tool digests are spliced
// into old tool rounds first, and the newest committed summary turn anchors
// the fit: everything before it is represented and never re-read. Summaries
// are produced in the background (compaction.go) and only ever read back
// here, so the same inputs still give a byte-identical prompt.
func (compactStage) Run(ctx context.Context, b *Build) error {
	sections := sectionBodies(b)
	b.sectionCache = sections

	window := b.Budget.ContextWindow - b.Budget.ReservedOutput
	if window <= 0 {
		return kernel.NewError(kernel.CodeInvalid, "the context window leaves no room for input").
			WithPath(b.Snapshot.Conversation.ID)
	}

	fixed, err := b.tokenizer.Count(b.input.Model, sectionParts(sections))
	if err != nil {
		return kernel.WrapError(kernel.CodeInternal, "the tokenizer failed on the fixed sections", err)
	}
	if fixed > window {
		return kernel.NewError(kernel.CodeInvalid, "the fixed sections alone exceed the token budget").
			WithPath(b.Snapshot.Conversation.ID)
	}

	// Committed digests replace old tool bodies before anything is costed,
	// so the fit and the assembled messages agree on the same bytes.
	if digests := DigestsFromTurns(b.Transcript); len(digests) > 0 {
		b.Transcript = SpliceToolDigests(b.Transcript, digests)
	}

	// The newest committed summary turn anchors the fit: only the anchor
	// itself is kept from its prefix — everything before it is represented
	// by the summary and never re-read.
	var anchored []domain.Turn
	if ai, ok := FindSummaryAnchor(b.Transcript); ok {
		b.checkSummaryCovers(b.Transcript[ai], b.Transcript[:ai])
		anchored = b.Transcript[ai : ai+1]
		b.Transcript = b.Transcript[ai+1:]
	}

	// The post-anchor tail is fitted newest first, and a tool call and its
	// result are kept or dropped together, so they are paired into units
	// first.
	units := pairToolCalls(b.Transcript)
	costFn := func(ts []domain.Turn) (int, error) {
		return b.tokenizer.Count(b.input.Model, messageParts(ctx, b, ts))
	}
	kept, omitted := fitUnits(units, costFn, window-fixed)
	b.Transcript = append(anchored, kept...)

	if len(omitted) > 0 {
		b.diagnostics("transcript.omitted",
			fmt.Sprintf("%d turns after the last summary anchor did not fit the budget", len(omitted)), nil)
	}

	used, err := b.tokenizer.Count(b.input.Model, messageParts(ctx, b, b.Transcript))
	if err != nil {
		return kernel.WrapError(kernel.CodeInternal, "the tokenizer failed on the transcript", err)
	}
	b.Budget.Used = fixed + used
	return nil
}

// checkSummaryCovers validates a summary anchor against the prefix it claims
// to represent. A cover that does not resolve to a turn before the anchor is
// a diagnostic, never an error: the anchor still holds, the caller just
// learns that the bookkeeping drifted.
func (b *Build) checkSummaryCovers(anchor domain.Turn, prefix []domain.Turn) {
	for _, blk := range anchor.Blocks {
		if blk.Type != "summary" {
			continue
		}
		text, covers, ok := SummaryCovers(blk)
		if !ok {
			b.diagnostics("summary.covers", "the summary anchor has no readable payload", nil)
			continue
		}
		if strings.TrimSpace(text) == "" {
			b.diagnostics("summary.covers", "the summary anchor carries no text", nil)
		}
		present := make(map[string]bool, len(prefix))
		for _, t := range prefix {
			present[t.ID] = true
		}
		for _, ref := range covers {
			if !present[ref.ID] {
				b.diagnostics("summary.covers", "the summary covers turn "+ref.ID+", which is not in the prefix it anchors", nil)
			}
		}
	}
}

// pairToolCalls groups turns into units, where a unit is a turn plus the
// turns carrying the results of the tool calls it requested. A call without
// a committed result is its own unit, which is what the agent loop re-runs
// after a restart.
func pairToolCalls(turns []domain.Turn) [][]domain.Turn {
	used := make([]bool, len(turns))
	var units [][]domain.Turn

	for i, t := range turns {
		if used[i] {
			continue
		}
		used[i] = true
		unit := []domain.Turn{t}
		for _, p := range turnParts(t) {
			call, ok := p.(provider.ToolCallPart)
			if !ok {
				continue
			}
			for j := i + 1; j < len(turns); j++ {
				if used[j] || !turnHasToolResult(turns[j], call.CallID) {
					continue
				}
				used[j] = true
				unit = append(unit, turns[j])
				break
			}
		}
		units = append(units, unit)
	}
	return units
}

func turnHasToolResult(t domain.Turn, callID string) bool {
	for _, p := range turnParts(t) {
		if res, ok := p.(provider.ToolResultPart); ok && res.CallID == callID {
			return true
		}
	}
	return false
}

// fitUnits keeps whole units newest first until the budget is spent, and
// returns the omitted turns in their original order so a compactor sees them
// chronologically. cost returns the token cost of one unit; the stage wires
// it to the tokenizer over the build's own block mapping.
func fitUnits(units [][]domain.Turn, cost func([]domain.Turn) (int, error), budget int) (kept, omitted []domain.Turn) {
	if budget <= 0 {
		for _, u := range units {
			omitted = append(omitted, u...)
		}
		return nil, omitted
	}

	include := make([]bool, len(units))
	total := 0
	for i := len(units) - 1; i >= 0; i-- {
		unitCost, err := cost(units[i])
		if err != nil {
			break
		}
		if total+unitCost > budget {
			break
		}
		include[i] = true
		total += unitCost
	}

	for i, u := range units {
		if include[i] {
			kept = append(kept, u...)
		} else {
			omitted = append(omitted, u...)
		}
	}
	return kept, omitted
}

// --- stage 5: assemble ---

type assembleStage struct{}

func (assembleStage) Name() string { return "assemble" }

// Run builds the ordered system sections and the transcript messages.
func (assembleStage) Run(ctx context.Context, b *Build) error {
	if b.sectionCache == nil {
		b.sectionCache = sectionBodies(b)
	}
	b.Sections = b.sectionCache
	b.Request.System = sectionParts(b.Sections)
	b.Request.Messages = assembleMessages(ctx, b, b.Transcript)
	return nil
}

// --- stage 6: map ---

type mapStage struct{}

func (mapStage) Name() string { return "map" }

// Run converts blocks to provider parts, applies capability downgrades and
// attaches tool specs and an output schema when needed. The block-to-part
// conversion already happened while messages were assembled, because a
// provider.Message is made of parts; this stage finishes the request.
func (mapStage) Run(ctx context.Context, b *Build) error {
	b.Request.Model = b.input.Model
	b.Request.MaxOutputTokens = b.input.MaxOutputTokens
	applyConstraints(b)

	if b.input.Structured {
		// Structured-output mode: the schema is attached and the model is
		// told to answer in JSON blocks; tool specs already went into a
		// system section.
		b.Request.OutputSchema = b.input.OutputSchema
		return nil
	}
	// Native tool calling is the default: specs go in the request, not a
	// system section.
	b.Request.Tools = b.input.Tools
	return nil
}

// applyConstraints folds the stored and caller-supplied constraints into the
// request, in section 6's precedence order: role, then conversation, then the
// caller (the last user turn). Every field the later source sets wins; a
// source that is absent or does not decode contributes nothing.
func applyConstraints(b *Build) {
	var merged domain.Constraints
	apply := func(raw json.RawMessage) {
		if len(bytes.TrimSpace(raw)) == 0 {
			return
		}
		c, err := domain.DecodeConstraints(raw)
		if err != nil {
			b.diagnostics("constraints.invalid", "stored constraints do not decode: "+err.Error(), nil)
			return
		}
		merged = domain.MergeConstraints(merged, c)
	}
	if b.Snapshot.Role != nil {
		apply(b.Snapshot.Role.Constraints)
	}
	apply(b.Snapshot.Conversation.Constraints)
	if b.input.Constraints != nil {
		merged = domain.MergeConstraints(merged, *b.input.Constraints)
	}
	if merged.Temperature != nil {
		b.Request.Temperature = merged.Temperature
	}
	if merged.MaxOutputTokens != nil && b.Request.MaxOutputTokens == 0 {
		b.Request.MaxOutputTokens = *merged.MaxOutputTokens
	}
	if len(merged.Stop) > 0 {
		b.Request.Stop = merged.Stop
	}
	if merged.ThinkingBudget != nil {
		b.diagnostics("constraints.unsupported", "thinking_budget has no provider-wide mapping yet; it is recorded but not applied", nil)
	}
}

// --- assembly helpers ---

// sectionBodies builds the system sections in section 9's order. It is a
// pure function of the build, so the compact and assemble stages agree on
// one deterministic result. It runs exactly once per build (the compact
// stage fills the cache and the assemble stage reuses it), so the anchor
// diagnostics it records are recorded once too.
func sectionBodies(b *Build) []Section {
	var out []Section
	add := func(id, title string, order int, body string) {
		if strings.TrimSpace(body) == "" {
			return
		}
		out = append(out, Section{ID: id, Title: title, Body: body, Order: order})
	}
	addTitled := func(id, title string, order int, titledBody string) {
		// Sections whose body carries its own markdown headers (the TS
		// structured preamble and tools body) are added with no title, so
		// Section.Content renders the body verbatim.
		add(id, "", order, titledBody)
	}

	// Structured-output mode opens with the workspace operating system
	// preamble; hybrid mode does not (section 9's policy section is always
	// present in both).
	if b.input.Structured {
		addTitled("operating-system", "", orderOperatingSystem, structuredOSBody())
	}

	// Policy is fixed operating rules from the workspace and is always
	// present when supplied.
	add("policy", "Policy", orderPolicy, b.input.Policy)

	// Tools are described in a system section in structured-output mode
	// only; native mode passes them in the request (section 9's table).
	if b.input.Structured {
		addTitled("tools", "", orderTools, toolSectionBody(b.input.Tools))
		addTitled("blocks", "", orderBlocks, structuredBlocksBody(b.input.Content))
	} else if b.input.Content != nil {
		// Hybrid mode always teaches the block markup, generated from the
		// registry, so the model's prose carries parseable blocks.
		add("blocks", "Block types", orderBlocks, hybridBlocksBody(b.input.Content, b.input.NativeTools))
	}

	if b.Snapshot.Role != nil {
		add("persona", "Persona", orderPersona, b.Snapshot.Role.Persona)
	}

	add("preferences", "User Preferences", orderPreferences, preferencesBody(b))
	add("context", "Context", orderContext, contextBody(b))
	add("artifacts", "Artifacts", orderArtifacts, artifactsBody(b))

	instructions := b.input.Instructions
	if instructions == "" {
		instructions = b.Snapshot.Settings.Prompt
	}
	add("instructions", "Instructions", orderInstructions, instructions)

	// Extensions anchor against the built-ins that were actually assembled:
	// before/after shifts one order slot, which always lands on the right
	// side of the anchor (a collision with the neighbouring built-in is
	// broken by the ID tie-break in the sort below).
	present := make(map[string]int, len(out))
	for _, s := range out {
		present[s.ID] = s.Order
	}
	for _, e := range b.input.Extensions {
		if strings.TrimSpace(e.Content) == "" {
			continue
		}
		order, code, msg := extensionOrder(e, present)
		if msg != "" {
			b.diagnostics(code, msg, nil)
		}
		out = append(out, Section{ID: e.Label, Title: e.Label, Body: e.Content, Order: order})
	}

	// Sections sort by order then ID, so assembly is deterministic and
	// nothing depends on the clock or on map iteration.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// extensionOrder resolves one extension's Position against the assembled
// sections, returning a diagnostic code and message when the position could
// not be honoured as written.
func extensionOrder(e Extension, present map[string]int) (order int, code, msg string) {
	switch pos := e.Position; {
	case pos == "prepend":
		return orderExtensionFirst, "", ""
	case pos == "append":
		return orderExtensionLast, "", ""
	case strings.HasPrefix(pos, "before:"):
		return anchorOrder(pos[len("before:"):], present, -1)
	case strings.HasPrefix(pos, "after:"):
		return anchorOrder(pos[len("after:"):], present, +1)
	default:
		return orderExtensionLast, "extension.position",
			fmt.Sprintf("unrecognized extension position %q; appending to end", pos)
	}
}

func anchorOrder(id string, present map[string]int, delta int) (order int, code, msg string) {
	base, ok := present[id]
	if !ok {
		return orderExtensionLast, "extension.anchor",
			fmt.Sprintf("extension anchor %q not found; appending to end", id)
	}
	return base + delta, "", ""
}

// preferencesBody folds the resolved preference blocks into TS-style
// bullets. Section 6: "Preferences are content of type preference"; the
// source tier does not matter here because resolve already picked one
// winner per key.
func preferencesBody(b *Build) string {
	var lines []string
	for _, it := range b.resolved {
		if it.Content.Type != "preference" {
			continue
		}
		text := preferenceText(it.Content)
		if text == "" {
			continue
		}
		lines = append(lines, "- "+text)
	}
	return strings.Join(lines, "\n")
}

func preferenceText(c domain.Content) string {
	var m map[string]any
	if json.Unmarshal(c.Value, &m) == nil {
		if txt, ok := m["text"].(string); ok {
			return txt
		}
	}
	return ""
}

// contextBody is content in read mode (section 9's table). Preferences are
// read-mode content too, but they have their own section, so they are
// excluded here.
func contextBody(b *Build) string {
	var parts []string
	for _, it := range b.resolved {
		if it.Content.Mode != domain.ModeRead || it.Content.Type == "preference" {
			continue
		}
		parts = append(parts, renderContent(b, it.Content))
	}
	return strings.Join(parts, "\n\n")
}

// artifactsBody is write and readwrite content (section 9's table). With
// native tool calling the body is one read away, so the section carries only
// a manifest — key, version, mode and title — which keeps the section tiny
// and its prefix cache-stable across edits. Without tools (the no-tools
// structured path) inline bodies remain the only channel, so they render in
// full.
func artifactsBody(b *Build) string {
	var parts []string
	for _, it := range b.resolved {
		if it.Content.Mode != domain.ModeWrite && it.Content.Mode != domain.ModeReadWrite {
			continue
		}
		if b.input.NativeTools {
			parts = append(parts, artifactManifestLine(it.Content))
			continue
		}
		parts = append(parts, renderContent(b, it.Content))
	}
	return strings.Join(parts, "\n\n")
}

// artifactManifestLine renders one content entry as a manifest line.
func artifactManifestLine(c domain.Content) string {
	title := contentTitle(c)
	return fmt.Sprintf("- %s v%d (%s): %s", c.Key, c.Version, c.Mode, title)
}

// contentTitle extracts an entry's title when its payload carries one, and
// falls back to the first line of its rendered body.
func contentTitle(c domain.Content) string {
	var m map[string]any
	if json.Unmarshal(c.Value, &m) == nil {
		if t, ok := m["title"].(string); ok && t != "" {
			return t
		}
	}
	line := contentBody(c)
	if i := strings.IndexAny(line, "\n\r"); i >= 0 {
		line = line[:i]
	}
	if len(line) > 60 {
		line = line[:60] + "..."
	}
	return line
}

// renderContent renders one content entry through the block registry when
// one is wired (each type's Render), and falls back to the raw text bodies
// otherwise. A registry failure is a diagnostic, never a dropped section.
func renderContent(b *Build, c domain.Content) string {
	reg := b.input.Content
	if reg != nil {
		if bt, ok := reg.Lookup(c.Type); ok && bt.Capabilities()&content.Consumable != 0 {
			var payload map[string]any
			if err := json.Unmarshal(c.Value, &payload); err == nil {
				value, _ := bt.Decode(payload)
				if value != nil {
					if s, rerr := bt.Render(value); rerr == nil {
						return s
					}
				}
			}
			ref := c.Ref()
			b.diagnostics("content.render", "the registry could not render "+c.Key+" ("+c.Type+"), using the raw body", &ref)
		}
	}
	return contentBody(c)
}

func contentBody(c domain.Content) string {
	if txt, ok := valueText(c.Value); ok {
		return txt
	}
	return string(c.Value)
}

// structuredOSBody is the structured-output preamble, verbatim from the TS
// assembler's OPERATING_SYSTEM_CONTENT.
func structuredOSBody() string {
	return strings.TrimSpace(`
# WORKSPACE OPERATING SYSTEM
You are operating within a structured workspace. Your output MUST be a JSON object matching the provided schema.
Your entire response MUST be valid JSON. Nothing else.
`)
}

func toolSectionBody(specs []provider.ToolSpec) string {
	var sb strings.Builder
	for _, s := range specs {
		sb.WriteString("## " + s.Name + "\n")
		if s.Description != "" {
			sb.WriteString(s.Description + "\n")
		}
		if len(s.Schema) > 0 {
			sb.WriteString("```json\n" + string(s.Schema) + "\n```\n")
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// structuredBlocksBody describes the block types the model may emit as JSON
// objects in structured-output mode, generated from the registry when one
// is wired (a stale static body otherwise, pinned by the earlier goldens).
func structuredBlocksBody(reg content.Registry) string {
	var sb strings.Builder
	sb.WriteString("# BLOCK TYPES & USAGE RULES\n")
	sb.WriteString("Your response MUST be a JSON object with a `blocks` array.\n")
	sb.WriteString("Each element of `blocks` must be one of the following types:\n\n")
	if reg == nil {
		sb.WriteString("text: plain or markdown text\n\njson: structured data\n\n")
		sb.WriteString("## BLOCK CONSTRAINTS\n")
		sb.WriteString("- Output must be **valid JSON only** — no prose before or after the JSON object.\n")
		sb.WriteString("- Responses may contain multiple blocks. Blocks are rendered in order.\n")
		sb.WriteString("- **Do NOT generate IDs.** The workspace is solely responsible for block ID assignment.\n")
		sb.WriteString("- Do not infer or invent fields not defined in the block schemas above.\n")
		return strings.TrimRight(sb.String(), "\n")
	}
	for _, bt := range reg.Types() {
		if bt.Capabilities()&content.Emittable == 0 || bt.Name() == "thinking" || bt.Name() == "tool_call" || bt.Name() == "unknown" {
			// tool_call blocks stay native; unknown is never emittable;
			// thinking is a provider channel — traces come back from the
			// model's reasoning, they are never asked for in prose.
			continue
		}
		sb.WriteString(bt.Name())
		if d := bt.Describe(); d != "" {
			sb.WriteString(": " + d)
		}
		sb.WriteString("\n")
		for _, r := range bt.Rules() {
			sb.WriteString("- " + r + "\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("## BLOCK CONSTRAINTS\n")
	sb.WriteString("- Output must be **valid JSON only** — no prose before or after the JSON object.\n")
	sb.WriteString("- Responses may contain multiple blocks. Blocks are rendered in order.\n")
	sb.WriteString("- **Do NOT generate IDs.** The workspace is solely responsible for block ID assignment.\n")
	sb.WriteString("- Do not infer or invent fields not defined in the block schemas above.\n")
	return strings.TrimRight(sb.String(), "\n")
}

// hybridBlocksBody teaches the model the prose-plus-markup contract: ordinary
// text with ```workspace:<type> fenced blocks, generated from the registry's
// emittable types.
func hybridBlocksBody(reg content.Registry, nativeTools bool) string {
	var sb strings.Builder
	sb.WriteString("Reply in ordinary text. When a message needs typed content, embed a workspace block like this:\n\n")
	sb.WriteString("```workspace:<type>\n")
	sb.WriteString("{...one JSON object for the block value...}\n")
	sb.WriteString("```\n\n")
	sb.WriteString("Everything outside such fences is plain text. The workspace assigns block IDs; do not emit `id` or `key`.\n\n")
	sb.WriteString("Each payload is exactly one JSON object with the fields its type lists; unknown fields are rejected.\n\n")
	sb.WriteString("## BLOCK TYPES\n")
	for _, bt := range reg.Types() {
		if bt.Capabilities()&content.Emittable == 0 {
			continue
		}
		switch bt.Name() {
		case "text":
			// Prose is a text block; no wrapper needed or allowed.
			continue
		case "thinking":
			// Thinking is the provider's channel: traces come back from the
			// model's reasoning channel and are stored for display. Asking
			// for them in prose pollutes the user-visible transcript, and a
			// thinking-capable model already has a native route.
			continue
		case "tool_call":
			if nativeTools {
				// Tools are invoked through the provider's native channel.
				continue
			}
		case "unknown":
			continue
		}
		sb.WriteString("- `" + bt.Name() + "`")
		if d := bt.Describe(); d != "" {
			sb.WriteString(": " + d)
		}
		sb.WriteString("\n")
		for _, r := range bt.Rules() {
			sb.WriteString("  - " + r + "\n")
		}
	}
	sb.WriteString("\nOnly the types listed above may appear in a workspace fence. Never wrap tools in blocks;")
	if nativeTools {
		sb.WriteString(" call tools through the provider's tool channel instead.\n")
	} else {
		sb.WriteString(" use `workspace:tool_call` blocks for tool calls.\n")
	}
	return strings.TrimSpace(sb.String())
}

// systemText joins the sections into the single system string the TS
// assembler produces; empty sections are filtered.
func systemText(sections []Section) string {
	lines := make([]string, 0, len(sections))
	for _, s := range sections {
		if c := s.Content(); strings.TrimSpace(c) != "" {
			lines = append(lines, c)
		}
	}
	return strings.Join(lines, SystemSeparator)
}

// sectionParts renders the sections as the system parts a provider
// receives: one joined part, matching the TS assembler's single-string
// system prompt.
func sectionParts(sections []Section) []provider.Part {
	if len(sections) == 0 {
		return nil
	}
	return []provider.Part{provider.TextPart{Text: systemText(sections)}}
}

func turnMessages(turns []domain.Turn) []provider.Message {
	msgs := make([]provider.Message, 0, len(turns))
	for _, t := range turns {
		msgs = append(msgs, provider.Message{Role: roleOf(t.Actor.Kind), Parts: turnParts(t)})
	}
	return msgs
}

// assembleMessages and messageParts are the stage-level forms: they map
// blocks through the build's block registry when one is wired (per type and
// provider mapper, with capability downgrades), falling back to the static
// mapping above.
func assembleMessages(ctx context.Context, b *Build, turns []domain.Turn) []provider.Message {
	msgs := make([]provider.Message, 0, len(turns))
	for _, t := range turns {
		msgs = append(msgs, provider.Message{Role: roleOf(t.Actor.Kind), Parts: buildTurnParts(ctx, b, t)})
	}
	return msgs
}

func messageParts(ctx context.Context, b *Build, turns []domain.Turn) []provider.Part {
	var parts []provider.Part
	for _, t := range turns {
		parts = append(parts, buildTurnParts(ctx, b, t)...)
	}
	return parts
}

func buildTurnParts(ctx context.Context, b *Build, t domain.Turn) []provider.Part {
	parts := make([]provider.Part, 0, len(t.Blocks))
	for _, blk := range t.Blocks {
		parts = append(parts, buildBlockParts(ctx, b, blk)...)
	}
	return parts
}

// buildBlockParts maps one block through the registry: MapperFor(type,
// provider), downgraded by the model's capabilities. A mapper error is a
// diagnostic plus a text marker — never a dropped block. Types with no
// mapper (unknown) fall through to the static mapping.
func buildBlockParts(ctx context.Context, b *Build, blk domain.Block) []provider.Part {
	reg := b.input.Content
	if reg != nil {
		if m, ok := reg.MapperFor(blk.Type, b.input.ProviderName); ok {
			env := content.MapEnv{Blobs: b.input.Blobs, Owner: b.input.Snapshot.Conversation.Owner}
			parts, err := m.ToParts(ctx, blk, b.input.Capabilities, env)
			if err != nil {
				b.diagnostics("block.mapper", fmt.Sprintf("the %s mapper failed for a %s block: %v", b.input.ProviderName, blk.Type, err), nil)
				return []provider.Part{provider.TextPart{Text: "[" + blk.Type + " omitted]"}}
			}
			if len(parts) > 0 {
				return parts
			}
			// A nil result is the mapper's way of dropping a block the
			// provider cannot take (thinking without a thinking channel).
			return nil
		}
	}
	return blockPart(blk)
}

func roleOf(k domain.ActorKind) provider.Role {
	switch k {
	case domain.ActorAgent:
		return provider.AssistantRole
	case domain.ActorTool:
		return provider.ToolRole
	default:
		// provider.Role has no system role, so a system turn is presented as
		// user-level context: a capability downgrade, not a loss of history.
		return provider.UserRole
	}
}

// turnParts maps a turn's blocks to provider parts, applying capability
// downgrades for blocks a provider cannot take as-is.
func turnParts(t domain.Turn) []provider.Part {
	parts := make([]provider.Part, 0, len(t.Blocks))
	for _, blk := range t.Blocks {
		parts = append(parts, blockPart(blk)...)
	}
	return parts
}

func blockPart(blk domain.Block) []provider.Part {
	switch blk.Type {
	case "text":
		if txt, ok := blockText(blk); ok {
			return []provider.Part{provider.TextPart{Text: txt}}
		}
	case "json":
		raw := blk.Raw
		if len(raw) == 0 && blk.Value != nil {
			if m, err := json.Marshal(blk.Value); err == nil {
				raw = m
			}
		}
		return []provider.Part{provider.TextPart{Text: string(raw)}}
	case "thinking":
		// Section 6's block table: stored for display, not sent back by
		// default, so the raw fallback below must not leak a thought into
		// the transcript as text.
		return nil
	case "image", "file":
		// Capability downgrade: without a resolvable Source the bytes cannot
		// be sent, so the block is represented by a marker rather than
		// silently vanishing from the transcript.
		return []provider.Part{provider.TextPart{Text: "[" + blk.Type + " omitted]"}}
	case "tool_call":
		if call, ok := toolCallFrom(blk); ok {
			return []provider.Part{call}
		}
	case "tool_result":
		if res, ok := toolResultFrom(blk); ok {
			return []provider.Part{res}
		}
	case "summary":
		// A committed history digest renders as plain text; it anchors the
		// fit and leads the transcript it represents.
		if txt, ok := blockText(blk); ok {
			return []provider.Part{provider.TextPart{Text: txt}}
		}
	case "tool_digest":
		// Workspace bookkeeping: never sent to a provider. Its text is
		// spliced into the old tool result it digests by the compact stage.
		return nil
	}
	if len(blk.Raw) > 0 {
		return []provider.Part{provider.TextPart{Text: string(blk.Raw)}}
	}
	return nil
}

func blockText(blk domain.Block) (string, bool) {
	if txt, ok := valueText(blk.Value); ok {
		return txt, true
	}
	if len(blk.Raw) > 0 {
		var m map[string]any
		if json.Unmarshal(blk.Raw, &m) == nil {
			if txt, ok := valueText(m); ok {
				return txt, true
			}
		}
	}
	return "", false
}

func valueText(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	txt, ok := m["text"].(string)
	return txt, ok
}

// toolCallFrom and toolResultFrom decode the two tool block shapes. The
// agent loop commits them as blocks, and this is where they become the
// provider parts a model needs to continue a tool call.
func toolCallFrom(blk domain.Block) (provider.ToolCallPart, bool) {
	raw := blockObject(blk)
	if raw == nil {
		return provider.ToolCallPart{}, false
	}
	callID, _ := raw["call_id"].(string)
	name, _ := raw["name"].(string)
	if callID == "" || name == "" {
		return provider.ToolCallPart{}, false
	}
	args, _ := raw["arguments"].(json.RawMessage)
	if len(args) == 0 {
		if a, ok := raw["arguments"]; ok {
			if m, err := json.Marshal(a); err == nil {
				args = m
			}
		}
	}
	return provider.ToolCallPart{CallID: callID, Name: name, Arguments: args}, true
}

func toolResultFrom(blk domain.Block) (provider.ToolResultPart, bool) {
	raw := blockObject(blk)
	if raw == nil {
		return provider.ToolResultPart{}, false
	}
	callID, _ := raw["call_id"].(string)
	if callID == "" {
		return provider.ToolResultPart{}, false
	}
	isError, _ := raw["is_error"].(bool)
	out, _ := raw["output"].(json.RawMessage)
	if len(out) == 0 {
		if o, ok := raw["output"]; ok {
			if m, err := json.Marshal(o); err == nil {
				out = m
			}
		}
	}
	return provider.ToolResultPart{CallID: callID, Output: out, IsError: isError}, true
}

// blockObject returns the block's payload as a generic object, preferring the
// typed Value and falling back to the original bytes.
func blockObject(blk domain.Block) map[string]any {
	if m, ok := blk.Value.(map[string]any); ok {
		return m
	}
	if len(blk.Raw) > 0 {
		var m map[string]any
		if json.Unmarshal(blk.Raw, &m) == nil {
			return m
		}
	}
	return nil
}
