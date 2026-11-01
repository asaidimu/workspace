package prompt

import (
	"context"
	"encoding/json"

	"github.com/asaidimu/workspace/domain"
)

// Compaction is a background concern, not a build-time one: a build never
// calls a model, so prompts stay byte-identical for identical inputs. The
// compactor sees the full snapshot in the background and returns summary
// candidates; a worker validates them and commits them through the kernel
// (a ContextSummary becomes a system turn carrying a summary block, a
// ToolUseSummary becomes a system turn carrying tool_digest blocks). Later
// builds only ever read committed state.

// CompactionKind tags a CompactionSummary as exactly one of its variants.
type CompactionKind string

const (
	// KindContext replaces a run of history turns with one prose digest.
	KindContext CompactionKind = "context"
	// KindToolUse replaces the bodies of old tool results with one-line
	// narratives keyed by call ID.
	KindToolUse CompactionKind = "tool_use"
)

// ContextSummary is a history digest: Text stands in for the turns listed in
// Covers, which stay in the tree for audit and branching but are never re-read
// into a prompt once the summary is committed.
type ContextSummary struct {
	Text   string
	Covers []domain.TurnRef
}

// ToolDigest is one committed narrative line for one old tool round: the
// action, its key parameters, and its outcome in domain terms.
type ToolDigest struct {
	CallID string
	Text   string
}

// ToolUseSummary collects digest lines for completed tool rounds. Lines are
// spliced into the bodies of the tool_result parts they cover; the
// call/result structure itself is kept, because provider APIs require a
// result for every call in the transcript.
type ToolUseSummary struct {
	Lines  []ToolDigest
	Covers []domain.TurnRef
}

// CompactionSummary is exactly one of ContextSummary or ToolUseSummary. A
// compactor returns any mix in one call. Unrecognized kinds are skipped with
// a diagnostic, never an error, so the union can grow without versioning.
type CompactionSummary struct {
	Kind    CompactionKind
	Context *ContextSummary
	ToolUse *ToolUseSummary
}

// ToolRound is one completed tool exchange in the transcript: the call, and
// the result it was answered with, if it has one. Output is the result body
// decoded to text.
type ToolRound struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
	Output    string
	IsError   bool
	// TurnRef points at the turn carrying the result (or the call itself,
	// for a round that never completed).
	TurnRef domain.TurnRef
}

// CompactionInput is the full picture a compactor may need to judge
// relevance. Compaction is a relevance judgment, and relevance is relative
// to the conversation: role, topics, what was already summarized, what
// content is linked. The elided turns are a hint, not a mandate — an
// implementation may summarize a different span, digest tool rounds instead
// of prose, or decline entirely.
type CompactionInput struct {
	// Snapshot is the consistent read of the conversation the summary is
	// for, including the FULL active chain, not just the tail.
	Snapshot Snapshot
	// Elided lists the turns the current fit would drop (chronological).
	// Empty when the transcript still fits.
	Elided []domain.Turn
	// ToolRounds lists the completed tool exchanges available for
	// digesting, oldest first.
	ToolRounds []ToolRound
	// LastAnchor is the newest committed summary turn, if any; everything
	// before it is already represented and must not be summarized again.
	LastAnchor *domain.TurnRef
	// Budget is the accounting that motivated the call; a worker that
	// cannot know the window leaves it zero.
	Budget Budget
}

// Compactor receives the full snapshot and returns summary candidates. The
// contract is deliberately minimal because compaction is finnicky: every
// required method is one more thing an implementation must fake. Templates,
// model choice, batching, granularity and trigger timing are implementation
// details, not parts of this interface. Compactors must be pure: they never
// mutate history; the caller validates and commits what they return.
type Compactor interface {
	Compact(ctx context.Context, in CompactionInput) ([]CompactionSummary, error)
}

// FindSummaryAnchor returns the index of the newest turn carrying a summary
// block. Everything before that turn is represented by it, so a build fits
// only the turns after it.
func FindSummaryAnchor(turns []domain.Turn) (int, bool) {
	anchor := -1
	for i, t := range turns {
		for _, blk := range t.Blocks {
			if blk.Type == "summary" {
				anchor = i
				break
			}
		}
	}
	return anchor, anchor >= 0
}

// SummaryCovers decodes a summary block's payload: the digest text and the
// turn refs it claims to represent.
func SummaryCovers(blk domain.Block) (string, []domain.TurnRef, bool) {
	m, ok := blk.Value.(map[string]any)
	if !ok {
		if len(blk.Raw) > 0 {
			if json.Unmarshal(blk.Raw, &m) != nil {
				return "", nil, false
			}
		} else {
			return "", nil, false
		}
	}
	text, _ := m["text"].(string)
	var covers []domain.TurnRef
	if raw, ok := m["covers"].([]any); ok {
		for _, c := range raw {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			id, _ := cm["id"].(string)
			version, _ := cm["version"].(float64)
			if id == "" {
				continue
			}
			covers = append(covers, domain.TurnRef{ID: id, Version: int(version)})
		}
	}
	return text, covers, true
}

// PairToolCalls groups turns into units, where a unit is a turn plus the
// turns carrying the results of the tool calls it requested. A call without
// a committed result is its own unit, which is what the agent loop re-runs
// after a restart. Exported for the background worker, which applies the
// same unit semantics the build does.
func PairToolCalls(turns []domain.Turn) [][]domain.Turn {
	return pairToolCalls(turns)
}

// CollectToolRounds returns the completed tool exchanges in a transcript,
// oldest first. A round is a tool_call block and the tool_result that
// answered it; a call with no committed result yields a round with empty
// Output and the call's own turn as its reference.
func CollectToolRounds(turns []domain.Turn) []ToolRound {
	var rounds []ToolRound
	byCall := map[string]*ToolRound{}
	var order []string
	for _, t := range turns {
		for _, blk := range t.Blocks {
			switch blk.Type {
			case "tool_call":
				if call, ok := toolCallFrom(blk); ok {
					rounds = append(rounds, ToolRound{
						CallID:    call.CallID,
						Name:      call.Name,
						Arguments: call.Arguments,
						TurnRef:   t.Ref(),
					})
					byCall[call.CallID] = &rounds[len(rounds)-1]
					order = append(order, call.CallID)
				}
			case "tool_result":
				if res, ok := toolResultFrom(blk); ok {
					if r, ok := byCall[res.CallID]; ok {
						r.Output = outputText(res.Output)
						r.IsError = res.IsError
						r.TurnRef = t.Ref()
					}
				}
			}
		}
	}
	return rounds
}

// outputText decodes a tool_result output payload to plain text: a JSON
// string is unquoted, anything else is kept as its raw bytes.
func outputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// DigestsFromTurns collects committed tool digests from a transcript's
// system turns, keyed by call ID. Digest turns carry no tool results, so
// collecting them is position independent.
func DigestsFromTurns(turns []domain.Turn) map[string]string {
	var digests map[string]string
	for _, t := range turns {
		for _, blk := range t.Blocks {
			if blk.Type != "tool_digest" {
				continue
			}
			m, ok := blk.Value.(map[string]any)
			if !ok && len(blk.Raw) > 0 {
				if json.Unmarshal(blk.Raw, &m) != nil {
					continue
				}
			}
			callID, _ := m["call_id"].(string)
			text, _ := m["text"].(string)
			if callID == "" || text == "" {
				continue
			}
			if digests == nil {
				digests = make(map[string]string)
			}
			digests[callID] = text
		}
	}
	return digests
}

// SpliceToolDigests rewrites old tool rounds in place of their raw bodies:
// every tool_result whose call ID has a digest becomes a result whose body
// is the digest line. The newest tool round is always left verbatim, and a
// call with no digest keeps its raw body — the transcript is never degraded
// into fragment markers. Turns are copied on write; the caller's slice is
// not mutated.
func SpliceToolDigests(turns []domain.Turn, digests map[string]string) []domain.Turn {
	if len(digests) == 0 {
		return turns
	}
	last := -1
	for i, t := range turns {
		for _, blk := range t.Blocks {
			if blk.Type == "tool_result" {
				last = i
			}
		}
	}
	changed := false
	out := make([]domain.Turn, len(turns))
	copy(out, turns)
	for i := 0; i < last; i++ {
		blocks, replaced := spliceTurnBlocks(turns[i], digests)
		if !replaced {
			continue
		}
		out[i].Blocks = blocks
		changed = true
	}
	if !changed {
		return turns
	}
	return out
}

// spliceTurnBlocks clones one turn's blocks, replacing splicable tool_result
// bodies with their digest lines. The copy is cheap: it runs at most once
// per spliced turn.
func spliceTurnBlocks(t domain.Turn, digests map[string]string) ([]domain.Block, bool) {
	var cloned []domain.Block
	replaced := false
	for bi, blk := range t.Blocks {
		if blk.Type != "tool_result" {
			continue
		}
		raw := blockObject(blk)
		if raw == nil {
			continue
		}
		callID, _ := raw["call_id"].(string)
		text, ok := digests[callID]
		if !ok || text == "" {
			continue
		}
		if !replaced {
			cloned = make([]domain.Block, len(t.Blocks))
			copy(cloned, t.Blocks)
		}
		isError, _ := raw["is_error"].(bool)
		payload := map[string]any{"call_id": callID, "output": text, "is_error": isError}
		rawBytes, _ := json.Marshal(payload)
		cloned[bi] = domain.Block{Type: "tool_result", Key: blk.Key, Value: payload, Raw: rawBytes}
		replaced = true
	}
	return cloned, replaced
}
