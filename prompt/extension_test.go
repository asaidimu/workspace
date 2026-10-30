package prompt

import (
	"context"
	"strings"
	"testing"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
)

// sectionIDs runs a full build (everything but map, which does not touch
// sections) and returns the assembled section IDs plus the diagnostics.
func sectionIDs(t *testing.T, in Input) ([]string, []Diagnostic) {
	t.Helper()
	var ids []string
	capture := captureStage{name: "capture", fn: func(ctx context.Context, b *Build) error {
		for _, s := range b.Sections {
			ids = append(ids, s.ID)
		}
		return nil
	}}
	p := NewPipeline(snapshotStage{}, selectStage{}, resolveStage{}, compactStage{}, assembleStage{}, capture)
	_, diags, err := p.Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return ids, diags
}

func joinIDs(ids []string) string { return strings.Join(ids, ",") }

func hasDiag(diags []Diagnostic, code, contains string) bool {
	for _, d := range diags {
		if d.Code == code && strings.Contains(d.Message, contains) {
			return true
		}
	}
	return false
}

func TestExtensionAnchorsLandAroundBuiltIns(t *testing.T) {
	in := baseInput()
	in.Snapshot.Role = &domain.Role{ID: "r1", Persona: "Persona."}
	in.Extensions = []Extension{
		{Label: "footer", Content: "Footer.", Position: "append"},
		{Label: "legal", Content: "Legal.", Position: "before:persona"},
		{Label: "company", Content: "Company.", Position: "prepend"},
		{Label: "postpol", Content: "Post policy.", Position: "after:policy"},
		{Label: "orphan", Content: "Orphan.", Position: "before:does-not-exist"},
	}

	ids, diags := sectionIDs(t, in)
	want := "company,policy,postpol,legal,persona,footer,orphan"
	if joinIDs(ids) != want {
		t.Fatalf("section order = %v, want %v", ids, want)
	}
	if !hasDiag(diags, "extension.anchor", "does-not-exist") {
		t.Fatalf("missing anchor diagnostic, got %v", diags)
	}
}

func TestExtensionAnchoringToAbsentSectionAppends(t *testing.T) {
	in := baseInput() // no role, so persona was never assembled
	in.Extensions = []Extension{
		{Label: "legal", Content: "Legal.", Position: "before:persona"},
	}
	ids, diags := sectionIDs(t, in)
	want := "policy,legal"
	if joinIDs(ids) != want {
		t.Fatalf("section order = %v, want %v", ids, want)
	}
	if !hasDiag(diags, "extension.anchor", "persona") {
		t.Fatalf("missing anchor diagnostic, got %v", diags)
	}
}

func TestExtensionUnrecognizedPositionAppends(t *testing.T) {
	in := baseInput()
	in.Extensions = []Extension{
		{Label: "loose", Content: "Loose.", Position: "sideways"},
	}
	ids, diags := sectionIDs(t, in)
	want := "policy,loose"
	if joinIDs(ids) != want {
		t.Fatalf("section order = %v, want %v", ids, want)
	}
	if !hasDiag(diags, "extension.position", "sideways") {
		t.Fatalf("missing position diagnostic, got %v", diags)
	}
}

func TestDuplicateExtensionLabelIsRejected(t *testing.T) {
	in := baseInput()
	in.Extensions = []Extension{
		{Label: "twice", Content: "A.", Position: "append"},
		{Label: "twice", Content: "B.", Position: "prepend"},
	}
	_, _, err := DefaultPipeline().Build(context.Background(), in)
	if err == nil {
		t.Fatal("duplicate labels were accepted")
	}
	if !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Fatalf("error = %v, want INVALID", err)
	}
}

func TestEmptyExtensionLabelIsRejected(t *testing.T) {
	in := baseInput()
	in.Extensions = []Extension{{Label: "", Content: "A.", Position: "append"}}
	_, _, err := DefaultPipeline().Build(context.Background(), in)
	if err == nil || !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
}

func TestEmptyExtensionContentIsSkipped(t *testing.T) {
	in := baseInput()
	in.Extensions = []Extension{
		{Label: "void", Content: "   ", Position: "append"},
	}
	ids, diags := sectionIDs(t, in)
	if joinIDs(ids) != "policy" {
		t.Fatalf("section order = %v, want just policy", ids)
	}
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "extension.") {
			t.Fatalf("empty content recorded a diagnostic: %v", d)
		}
	}
}

func TestExtensionCountsTowardTheFixedBudget(t *testing.T) {
	in := baseInput()
	in.ContextWindow = 24 // policy alone is tiny, plus a fat extension overflows
	in.MaxOutputTokens = 8
	in.Extensions = []Extension{
		{Label: "bulky", Content: strings.Repeat("words here ", 40), Position: "append"},
	}
	_, _, err := DefaultPipeline().Build(context.Background(), in)
	if err == nil {
		t.Fatal("oversized extension did not fail the budget")
	}
	if !kernel.IsCode(err, kernel.CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
}

func TestExtensionPlacementIsDeterministic(t *testing.T) {
	in := baseInput()
	in.Snapshot.Role = &domain.Role{ID: "r1", Persona: "Persona."}
	in.Extensions = []Extension{
		{Label: "a.first", Content: "One.", Position: "prepend"},
		{Label: "b.first", Content: "Two.", Position: "prepend"},
		{Label: "a.last", Content: "Three.", Position: "append"},
		{Label: "b.last", Content: "Four.", Position: "append"},
	}
	first, _ := sectionIDs(t, in)
	for i := 0; i < 20; i++ {
		again, _ := sectionIDs(t, in)
		if joinIDs(first) != joinIDs(again) {
			t.Fatalf("run %d differs: %v vs %v", i, again, first)
		}
	}
	want := "a.first,b.first,policy,persona,a.last,b.last"
	if joinIDs(first) != want {
		t.Fatalf("section order = %v, want %v", first, want)
	}
}
