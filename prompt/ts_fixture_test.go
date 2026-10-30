package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/provider"
)

// The TS fixtures are the transcript-mapping reference (see the pinned
// decision in todo/done/prompt-pipeline.md): they do NOT byte-match the system
// prompt, because the TS library emits the structured-output prompt that
// decision D5 defers. What they do pin is stage-6 mapping semantics —
// which blocks survive a trip through the mapper and how they are shaped.

// tsFixture mirrors testdata/ts-fixtures/prompt.json.
type tsFixture struct {
	Cases []tsPromptCase `json:"cases"`
}

type tsPromptCase struct {
	Name     string `json:"name"`
	Input    tsIn   `json:"input"`
	Expected tsExp  `json:"expected"`
}

type tsIn struct {
	Provider string   `json:"provider"`
	Prompt   tsPrompt `json:"prompt"`
}

type tsPrompt struct {
	Transcript []tsTurn  `json:"transcript"`
	Results    []tsBlock `json:"results"`
}

type tsTurn struct {
	ID      string    `json:"id"`
	Version int       `json:"version"`
	Actor   string    `json:"actor"`
	Blocks  []tsBlock `json:"blocks"`
}

type tsBlock struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type tsExp struct {
	Transcript []tsExpTurn `json:"transcript"`
}

type tsExpTurn struct {
	ID      string   `json:"id"`
	Version int      `json:"version"`
	Actor   string   `json:"actor"`
	Parts   []tsPart `json:"parts"`
}

type tsPart struct {
	Kind string          `json:"kind"`
	Text string          `json:"text"`
	Data json.RawMessage `json:"data"`
}

// normalized shapes both sides compare in.
type normPart struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type normMsg struct {
	Role  string     `json:"role"`
	Parts []normPart `json:"parts"`
}

func tsActorKind(actor string) domain.ActorKind {
	switch actor {
	case "assistant":
		return domain.ActorAgent
	case "tool":
		return domain.ActorTool
	case "system":
		return domain.ActorSystem
	default:
		return domain.ActorUser
	}
}

// tsBlockToDomain converts a fixture block into our domain block. text,
// thinking and json payloads keep (or become) the wrapper shape the mapper
// reads; Raw is left unset on purpose — the mapper resolves from Value, and
// with no Raw an unmapped block type is skipped, which is the TS behaviour
// the fixture pins.
func tsBlockToDomain(t tsBlock) domain.Block {
	var v map[string]any
	_ = json.Unmarshal(t.Value, &v)
	switch t.Type {
	case "json":
		if inner, ok := v["json"]; ok {
			v = toMap(inner)
		}
	}
	return domain.Block{Type: t.Type, Value: v}
}

func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// normalizeExpected removes the two deliberate divergences from the TS
// output, both documented in fixtures/gen-prompt.ts itself:
//
//  1. the TS assembler appends `results` to EVERY turn (the generator calls
//     it "a TS quirk"); in the domain a tool result is its own message, so
//     the appended copies are dropped;
//  2. a json part is a first-class `kind` in TS but the provider port has
//     no json part, so stage 6 carries the payload as JSON text.
//
// Everything else — text survives, thinking (internal) is dropped, order
// and roles are preserved — must match exactly.
func normalizeExpected(t *testing.T, exp tsExp, resultParts []normPart) []normMsg {
	t.Helper()
	out := make([]normMsg, 0, len(exp.Transcript))
	for _, turn := range exp.Transcript {
		m := normMsg{Role: tsRoleName(turn.Actor), Parts: []normPart{}}
		for _, p := range turn.Parts {
			np := normPart{Kind: p.Kind, Text: p.Text}
			if p.Kind == "json" {
				var v any
				if err := json.Unmarshal(p.Data, &v); err != nil {
					t.Fatalf("bad json part data: %v", err)
				}
				raw, err := json.Marshal(v)
				if err != nil {
					t.Fatalf("re-marshal json part: %v", err)
				}
				np.Kind, np.Text = "text", string(raw)
			}
			if containsNorm(resultParts, np) {
				continue // divergence 1: results appended to every turn
			}
			m.Parts = append(m.Parts, np)
		}
		out = append(out, m)
	}
	return out
}

func tsRoleName(actor string) string {
	if actor == "assistant" {
		return "assistant"
	}
	if actor == "tool" {
		return "tool"
	}
	return "user"
}

func containsNorm(hay []normPart, needle normPart) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func normalizeOurs(t *testing.T, msgs []provider.Message) []normMsg {
	t.Helper()
	out := make([]normMsg, 0, len(msgs))
	for _, m := range msgs {
		nm := normMsg{Role: roleName(m.Role), Parts: []normPart{}}
		for _, p := range m.Parts {
			switch v := p.(type) {
			case provider.TextPart:
				nm.Parts = append(nm.Parts, normPart{Kind: "text", Text: v.Text})
			default:
				t.Fatalf("unexpected part type %T in a fixture transcript", p)
			}
		}
		out = append(out, nm)
	}
	return out
}

func loadPromptFixture(t *testing.T) tsFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "ts-fixtures", "prompt.json"))
	if err != nil {
		t.Fatalf("read prompt fixture: %v", err)
	}
	var fx tsFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse prompt fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("prompt fixture has no cases")
	}
	return fx
}

// TestTranscriptMappingMatchesTSFixture replays every fixture case's
// transcript through our stage-6 mapping and compares it with the TS
// assembler's output after the two documented divergences above.
func TestTranscriptMappingMatchesTSFixture(t *testing.T) {
	for _, c := range loadPromptFixture(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			turns := make([]domain.Turn, 0, len(c.Input.Prompt.Transcript))
			for _, tt := range c.Input.Prompt.Transcript {
				turn := domain.Turn{ID: tt.ID, Version: tt.Version, Actor: domain.Actor{Kind: tsActorKind(tt.Actor), ID: tt.Actor}}
				for _, b := range tt.Blocks {
					turn.Blocks = append(turn.Blocks, tsBlockToDomain(b))
				}
				turns = append(turns, turn)
			}

			// What the TS assembler appended to every turn, normalized the
			// same way, so it can be recognized and stripped.
			resultTurn := domain.Turn{Blocks: make([]domain.Block, 0, len(c.Input.Prompt.Results))}
			for _, b := range c.Input.Prompt.Results {
				resultTurn.Blocks = append(resultTurn.Blocks, tsBlockToDomain(b))
			}
			var resultParts []normPart
			for _, m := range normalizeOurs(t, []provider.Message{{Role: provider.UserRole, Parts: turnParts(resultTurn)}}) {
				resultParts = append(resultParts, m.Parts...)
			}

			got := normalizeOurs(t, turnMessages(turns))
			want := normalizeExpected(t, c.Expected, resultParts)

			if !reflect.DeepEqual(got, want) {
				t.Errorf("transcript mapping diverged from the TS fixture\ngot:  %+v\nwant: %+v", got, want)
			}
			if len(got) != len(c.Input.Prompt.Transcript) {
				t.Errorf("message count = %d, want %d", len(got), len(c.Input.Prompt.Transcript))
			}
		})
	}
}
