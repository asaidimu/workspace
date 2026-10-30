package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
)

// stubTool is a Tool that only needs its spec.
type stubTool struct {
	spec ToolSpec
}

func (s stubTool) Spec() ToolSpec { return s.spec }
func (s stubTool) Call(ctx context.Context, p kernel.Principal, call ToolCall) (ToolResult, error) {
	return ToolResult{Output: json.RawMessage(`{}`)}, nil
}

func scopedTool(name string, scopes ...string) Tool {
	return stubTool{spec: ToolSpec{
		Name: name, Description: "d", Scopes: scopes,
		Schema: json.RawMessage(`{"type":"object"}`),
	}}
}

func TestRegisterAndLookupRoundTrip(t *testing.T) {
	r := NewRegistry()
	want := scopedTool("search")
	if err := r.Register(want); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("search")
	if !ok {
		t.Fatal("a registered tool must be found")
	}
	if got.Spec().Name != "search" {
		t.Fatalf("got tool %q", got.Spec().Name)
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Fatal("an unknown tool must not be found")
	}
}

func TestRegisterRejectsNilEmptyAndDuplicate(t *testing.T) {
	r := NewRegistry()
	if !kernel.IsCode(r.Register(nil), kernel.CodeInvalid) {
		t.Error("a nil tool must be INVALID")
	}
	if !kernel.IsCode(r.Register(stubTool{}), kernel.CodeInvalid) {
		t.Error("an empty name must be INVALID")
	}
	if err := r.Register(scopedTool("search")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if !kernel.IsCode(r.Register(scopedTool("search")), kernel.CodeConflict) {
		t.Error("a duplicate name must be CONFLICT")
	}
}

func TestSpecsFiltersByScopes(t *testing.T) {
	r := NewRegistry()
	for _, tool := range []Tool{
		scopedTool("open"),
		scopedTool("guarded", "fs:read"),
		scopedTool("guarded2", "fs:read", "fs:write"),
	} {
		if err := r.Register(tool); err != nil {
			t.Fatalf("Register %s: %v", tool.Spec().Name, err)
		}
	}

	cases := []struct {
		name   string
		scopes []string
		want   []string
	}{
		{"no scopes sees only the open tool", nil, []string{"open"}},
		{"one of two required scopes is not enough", []string{"fs:read"}, []string{"guarded", "open"}},
		{"every required scope is enough", []string{"fs:read", "fs:write"}, []string{"guarded", "guarded2", "open"}},
	}
	for _, c := range cases {
		p := kernel.Principal{Owner: "u1", Subject: "u1", Scopes: c.scopes}
		specs := r.Specs(p)
		got := make([]string, 0, len(specs))
		for _, s := range specs {
			got = append(got, s.Name)
		}
		if !equalStrings(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSpecsCarriesTheAdvertisedFields(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubTool{spec: ToolSpec{
		Name: "search", Description: "find things",
		Schema: json.RawMessage(`{"type":"object"}`),
	}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	specs := r.Specs(kernel.Principal{Owner: "u1", Subject: "u1"})
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	var want provider.ToolSpec = specs[0]
	if want.Name != "search" || want.Description != "find things" {
		t.Fatalf("spec = %+v", want)
	}
	if string(want.Schema) != `{"type":"object"}` {
		t.Fatalf("schema = %s", want.Schema)
	}
}

func TestSpecsIsSortedAndStable(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"zebra", "alpha", "mid"} {
		if err := r.Register(scopedTool(name)); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
	p := kernel.Principal{Owner: "u1", Subject: "u1"}
	first := namesOf(r.Specs(p))
	second := namesOf(r.Specs(p))
	if !equalStrings(first, []string{"alpha", "mid", "zebra"}) {
		t.Fatalf("not sorted by name: %v", first)
	}
	if !equalStrings(first, second) {
		t.Fatalf("not stable across calls: %v vs %v", first, second)
	}
}

func TestRegistryConcurrentRegistration(t *testing.T) {
	r := NewRegistry()
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Register(scopedTool(fmt.Sprintf("t%d", i))); err != nil {
				t.Errorf("Register: %v", err)
			}
		}()
	}
	wg.Wait()
	for i := range n {
		if _, ok := r.Lookup(fmt.Sprintf("t%d", i)); !ok {
			t.Errorf("t%d missing after concurrent registration", i)
		}
	}
	// Concurrent readers while a writer registers, so -race sees both sides.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for j := range 8 {
			if err := r.Register(scopedTool(fmt.Sprintf("w%d", j))); err != nil {
				t.Errorf("Register: %v", err)
			}
		}
	}()
	for j := range 64 {
		_ = r.Specs(kernel.Principal{Owner: "u", Subject: "u"})
		if j%8 == 0 {
			_, _ = r.Lookup("t0")
		}
	}
	<-done
}

func namesOf(specs []provider.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
