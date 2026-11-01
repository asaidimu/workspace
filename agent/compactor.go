package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/prompt"
	"github.com/asaidimu/workspace/provider"
)

// ModelCompactor digests omitted history and old tool traffic with a small
// secondary model through the provider interface. It is the default
// prompt.Compactor implementation and is deliberately plain: one call per
// compaction, two fixed templates (history digest, tool-line digest),
// temperature 0, a hard output-token cap.
//
// Guard rails are defaults of this implementation, not parts of the
// Compactor contract: refuse to summarize fewer than MinTurns turns, and
// show the digester only a head-truncated view of each tool output (the full
// text stays in the store regardless).
type ModelCompactor struct {
	Provider provider.Provider
	// Model is the small fast model that does the digesting.
	Model string
	// MinTurns refuses a history summary for fewer than this many omitted
	// turns — a digest of one or two turns costs more than it saves.
	// Default 4.
	MinTurns int
	// HeadLimit caps each tool output shown to the digester, in bytes.
	// Default 400.
	HeadLimit int
	// MaxOutputTokens caps the digester's reply. Default 512.
	MaxOutputTokens int
}

const compactorSystem = `You compress agent history for a workspace. Reply with ONE JSON object and nothing else:
{"context_summary": "<one short paragraph: what happened and what matters now; omit the field if no turns are given>",
 "tool_digests": [{"call_id": "<the call_id verbatim>", "text": "<one line: the action, its key parameters, its outcome in domain terms>"}]}
Omit fields with nothing to report. Never invent call_ids. No fragments of raw output — describe outcomes in words.`

// Compact implements prompt.Compactor. It is pure: nothing is committed and
// nothing in the input is mutated.
func (m *ModelCompactor) Compact(ctx context.Context, in prompt.CompactionInput) ([]prompt.CompactionSummary, error) {
	if m.Provider == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "the model compactor has no provider")
	}
	minTurns := m.MinTurns
	if minTurns <= 0 {
		minTurns = 4
	}
	head := m.HeadLimit
	if head <= 0 {
		head = 400
	}
	maxOut := m.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = 512
	}

	// Tool digests are cheap and always worth it when there is more than
	// one round; the newest round stays verbatim and is never digested.
	rounds := in.ToolRounds
	if len(rounds) > 1 {
		rounds = rounds[:len(rounds)-1]
	} else {
		rounds = nil
	}
	summarizeHistory := len(in.Omitted) >= minTurns
	if len(rounds) == 0 && !summarizeHistory {
		return nil, nil
	}

	var sb strings.Builder
	if summarizeHistory {
		sb.WriteString("TURNS (oldest first):\n")
		for _, t := range in.Omitted {
			for _, blk := range t.Blocks {
				if txt, ok := blockTextOf(blk); ok {
					sb.WriteString(string(t.Actor.Kind) + ": " + truncateRunes(txt, 200) + "\n")
				}
			}
		}
	}
	if len(rounds) > 0 {
		sb.WriteString("\nTOOL ROUNDS:\n")
		for _, r := range rounds {
			sb.WriteString("call_id=" + r.CallID + " tool=" + r.Name)
			if len(r.Arguments) > 0 {
				sb.WriteString(" arguments=" + truncateBytes(string(r.Arguments), 200))
			}
			sb.WriteString(" output=" + truncateBytes(r.Output, head))
			sb.WriteString("\n")
		}
	}

	req := provider.Request{
		Model:           m.Model,
		MaxOutputTokens: maxOut,
		Messages: []provider.Message{{
			Role:  provider.UserRole,
			Parts: []provider.Part{provider.TextPart{Text: compactorSystem + "\n\n" + sb.String()}},
		}},
	}
	resp, err := m.Provider.Generate(ctx, req)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeUnavailable, "the digester model failed", err)
	}

	out, ok := responseText(resp)
	if !ok {
		return nil, kernel.NewError(kernel.CodeInternal, "the digester model returned no text")
	}
	return parseCompaction(out, in)
}

// responseText joins a provider response's text parts into one string.
func responseText(resp provider.Response) (string, bool) {
	var sb strings.Builder
	for _, part := range resp.Message.Parts {
		if tp, ok := part.(provider.TextPart); ok {
			sb.WriteString(tp.Text)
		}
	}
	out := strings.TrimSpace(sb.String())
	return out, out != ""
}

// parseCompaction decodes the digester's JSON reply into summary candidates.
// Malformed or empty results come back as an error — the worker treats any
// compaction failure as a diagnostic and simply retries later.
func parseCompaction(out string, in prompt.CompactionInput) ([]prompt.CompactionSummary, error) {
	start := strings.IndexByte(out, '{')
	end := strings.LastIndexByte(out, '}')
	if start < 0 || end <= start {
		return nil, kernel.NewError(kernel.CodeInternal, "the digester reply contains no JSON object")
	}
	var parsed struct {
		ContextSummary string `json:"context_summary"`
		ToolDigests    []struct {
			CallID string `json:"call_id"`
			Text   string `json:"text"`
		} `json:"tool_digests"`
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &parsed); err != nil {
		return nil, kernel.WrapError(kernel.CodeInternal, "the digester reply is not valid JSON", err)
	}

	var out_summaries []prompt.CompactionSummary
	if text := strings.TrimSpace(parsed.ContextSummary); text != "" && len(in.Omitted) > 0 {
		covers := make([]domain.TurnRef, 0, len(in.Omitted))
		for _, t := range in.Omitted {
			covers = append(covers, t.Ref())
		}
		out_summaries = append(out_summaries, prompt.CompactionSummary{
			Kind:    prompt.KindContext,
			Context: &prompt.ContextSummary{Text: text, Covers: covers},
		})
	}
	if len(parsed.ToolDigests) > 0 {
		known := make(map[string]bool, len(in.ToolRounds))
		for _, r := range in.ToolRounds {
			known[r.CallID] = true
		}
		var lines []prompt.ToolDigest
		var covers []domain.TurnRef
		seenRef := map[domain.TurnRef]bool{}
		for _, d := range parsed.ToolDigests {
			text := strings.TrimSpace(d.Text)
			if d.CallID == "" || text == "" || !known[d.CallID] {
				continue // never invent call_ids
			}
			lines = append(lines, prompt.ToolDigest{CallID: d.CallID, Text: text})
			for _, r := range in.ToolRounds {
				if r.CallID == d.CallID && !seenRef[r.TurnRef] {
					seenRef[r.TurnRef] = true
					covers = append(covers, r.TurnRef)
				}
			}
		}
		if len(lines) > 0 {
			out_summaries = append(out_summaries, prompt.CompactionSummary{
				Kind:    prompt.KindToolUse,
				ToolUse: &prompt.ToolUseSummary{Lines: lines, Covers: covers},
			})
		}
	}
	return out_summaries, nil
}

// DigestWorker runs the compactor in the background and commits what it
// returns. The compactor is pure; this worker is the only writer: a
// ContextSummary becomes a system turn carrying a summary block, and a
// ToolUseSummary becomes a system turn carrying one tool_digest block per
// line. Later builds read only this committed state.
type DigestWorker struct {
	History History
	// Compactor produces the summary candidates. Required.
	Compactor prompt.Compactor
	// Pressure is the fraction of the working budget past which a history
	// summary is worth producing. Default 0.5.
	Pressure float64
}

// MaybeCompact reads the conversation, decides whether compaction is worth
// doing, calls the compactor and commits its candidates. It is safe to call
// after every step; when there is nothing to do it returns nil without
// writing. Failures surface to the caller but never corrupt history: the
// only writes are idempotent kernel appends.
func (w *DigestWorker) MaybeCompact(ctx context.Context, p kernel.Principal, conversationID string) ([]prompt.CompactionSummary, error) {
	if w == nil || w.Compactor == nil || w.History == nil {
		return nil, nil
	}
	snapshot, err := w.History.Read(ctx, p, conversationID)
	if err != nil {
		return nil, err
	}

	anchorIdx, anchored := prompt.FindSummaryAnchor(snapshot.Transcript)
	tail := snapshot.Transcript
	var lastAnchor *domain.TurnRef
	if anchored {
		tail = snapshot.Transcript[anchorIdx+1:]
		ref := snapshot.Transcript[anchorIdx].Ref()
		lastAnchor = &ref
	}

	// Omission policy, mirroring the build's unit semantics: when the tail's
	// estimated tokens pass the pressure threshold, propose a summary of the
	// oldest half of the tail's units. The build stays the authority — it
	// omits what does not fit regardless — this only seeds the summary so
	// the prefix stabilizes instead of wobbling.
	pressure := w.Pressure
	if pressure <= 0 {
		pressure = 0.5
	}
	var omitted []domain.Turn
	if estimateTurnTokens(tail) > int(float64(workingBudget(snapshot))*pressure) && len(tail) > 0 {
		units := prompt.PairToolCalls(tail)
		half := len(units) / 2
		if half > 0 {
			for _, u := range units[:half] {
				omitted = append(omitted, u...)
			}
		}
	}

	// Tool digests: every completed round except the newest (the newest
	// stays verbatim so the model keeps exact values for what it is
	// reasoning about now).
	rounds := prompt.CollectToolRounds(snapshot.Transcript)
	if len(rounds) > 1 {
		rounds = rounds[:len(rounds)-1]
	} else {
		rounds = nil
	}
	if len(omitted) == 0 && len(rounds) == 0 {
		return nil, nil
	}

	summaries, err := w.Compactor.Compact(ctx, prompt.CompactionInput{
		Snapshot:   snapshot,
		Omitted:    omitted,
		ToolRounds: rounds,
		LastAnchor: lastAnchor,
		Budget:     prompt.Budget{Used: estimateTurnTokens(tail)},
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	committed := make([]prompt.CompactionSummary, 0, len(summaries))
	for _, s := range summaries {
		switch {
		case s.Kind == prompt.KindContext && s.Context != nil && s.Context.Text != "":
			if err := w.commitContextSummary(ctx, p, conversationID, *s.Context); err != nil {
				return committed, err
			}
			committed = append(committed, s)
		case s.Kind == prompt.KindToolUse && s.ToolUse != nil && len(s.ToolUse.Lines) > 0:
			if err := w.commitToolSummary(ctx, p, conversationID, *s.ToolUse); err != nil {
				return committed, err
			}
			committed = append(committed, s)
		default:
			// An empty or unrecognized candidate is skipped: a compaction
			// failure is a diagnostic, never a corrupt commit.
		}
	}
	return committed, nil
}

// commitContextSummary appends a system turn carrying one summary block. The
// idempotency key is the hash of the covered turn refs, so a retried or
// racing compaction deduplicates instead of duplicating.
func (w *DigestWorker) commitContextSummary(ctx context.Context, p kernel.Principal, conversationID string, s prompt.ContextSummary) error {
	payload := map[string]any{
		"text":   s.Text,
		"covers": turnRefStrings(s.Covers),
		"origin": "compactor",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return kernel.WrapError(kernel.CodeInternal, "the summary payload does not marshal", err)
	}
	turn := domain.Turn{
		ID:      "sum-" + hashCovers(s.Covers),
		Version: 1,
		Actor:   domain.Actor{Kind: domain.ActorSystem, ID: "workspace"},
		Blocks:  []domain.Block{{Type: "summary", Value: payload, Raw: raw}},
	}
	_, err = w.History.Append(ctx, p, conversationID, turn, "summary:"+hashCovers(s.Covers))
	return err
}

// commitToolSummary appends a system turn carrying one tool_digest block per
// digest line.
func (w *DigestWorker) commitToolSummary(ctx context.Context, p kernel.Principal, conversationID string, s prompt.ToolUseSummary) error {
	blocks := make([]domain.Block, 0, len(s.Lines))
	keys := make([]string, 0, len(s.Lines))
	for _, l := range s.Lines {
		payload := map[string]any{"call_id": l.CallID, "text": l.Text}
		raw, err := json.Marshal(payload)
		if err != nil {
			return kernel.WrapError(kernel.CodeInternal, "the tool digest payload does not marshal", err)
		}
		blocks = append(blocks, domain.Block{Type: "tool_digest", Value: payload, Raw: raw})
		keys = append(keys, l.CallID)
	}
	turn := domain.Turn{
		ID:      "dig-" + hashStrings(keys),
		Version: 1,
		Actor:   domain.Actor{Kind: domain.ActorSystem, ID: "workspace"},
		Blocks:  blocks,
	}
	_, err := w.History.Append(ctx, p, conversationID, turn, "digest:"+hashStrings(keys))
	return err
}

// --- small shared helpers ---

func turnRefStrings(refs []domain.TurnRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s:%d", r.ID, r.Version))
	}
	return out
}

func hashCovers(refs []domain.TurnRef) string {
	return hashStrings(turnRefStrings(refs))
}

func hashStrings(ss []string) string {
	h := sha256Of(strings.Join(ss, ","))
	return h[:16]
}

func estimateTurnTokens(turns []domain.Turn) int {
	var n int
	for _, t := range turns {
		for _, blk := range t.Blocks {
			if txt, ok := blockTextOf(blk); ok {
				n += len(txt)
			}
		}
	}
	return n / 4
}

// workingBudget is the window the worker assumes when the snapshot does not
// name one: a conservative 128k minus 10 percent reserved output.
func workingBudget(s prompt.Snapshot) int {
	window := 128_000
	return window - window/10
}

func blockTextOf(blk domain.Block) (string, bool) {
	if m, ok := blk.Value.(map[string]any); ok {
		if txt, ok := m["text"].(string); ok && txt != "" {
			return txt, true
		}
	}
	if len(blk.Raw) > 0 {
		var m map[string]any
		if json.Unmarshal(blk.Raw, &m) == nil {
			if txt, ok := m["text"].(string); ok && txt != "" {
				return txt, true
			}
		}
	}
	return "", false
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
