package domain_test

// The turntree golden consumes testdata/ts-fixtures/turntree.json — the
// TypeScript package's turn-tree cases — as an explicit parity test for
// the Go store's tree semantics (Workspace specification, section 6).
// The fixture is a reference, not a byte-golden: where the TS shapes
// differ from the Go API the test maps them, and the divergences are
// listed at the bottom of this file.
//
// The fixture's chains are oldest-first; Chain walks newest-first, so
// every chain comparison reverses one of them.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/memory"
)

type fixtureTurn struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Actor   string `json:"actor"`
	Parent  *struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"parent"`
}

type fixtureCase struct {
	Name  string `json:"name"`
	Input struct {
		Turns []fixtureTurn `json:"turns"`
		Head  *struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		} `json:"head"`
	} `json:"input"`
	Expected struct {
		Head *struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		} `json:"head"`
		Chain []string `json:"chain"`
		Nodes map[string]struct {
			Version  int                 `json:"version"`
			Parent   json.RawMessage     `json:"parent"`
			Versions []int               `json:"versions"`
			Children map[string][]string `json:"children"`
			Actor    string              `json:"actor"`
		} `json:"nodes"`
		Queries []struct {
			Op     string          `json:"op"`
			Arg    string          `json:"arg"`
			Result json.RawMessage `json:"result"`
		} `json:"queries"`
	} `json:"expected"`
}

func TestTurnTreeFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "ts-fixtures", "turntree.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		Cases []fixtureCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			runTurnTreeCase(t, c)
		})
	}
}

func runTurnTreeCase(t *testing.T, c fixtureCase) {
	t.Helper()
	ctx := t.Context()
	st := memory.New(memory.Options{})
	repos, err := st.Scoped(store.Scope{Owner: "o1"})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}

	conv, err := repos.Conversations().Create(ctx, domain.Conversation{
		ID:    "sess-1",
		Owner: "o1",
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}

	// Append every fixture turn with its declared parent and version.
	// The store takes them verbatim; linking is the caller's job, which
	// is what the fixture is pinning.
	actors := map[string]domain.ActorKind{
		"user":      domain.ActorUser,
		"assistant": domain.ActorAgent,
		"system":    domain.ActorSystem,
	}
	for _, ft := range c.Input.Turns {
		turn := domain.Turn{
			ID:      ft.ID,
			Version: ft.Version,
			Owner:   "o1",
			Actor:   domain.Actor{Kind: actors[ft.Actor], ID: ft.Actor},
			Blocks: []domain.Block{
				{Type: "text", Key: ft.ID + "-b1", Value: map[string]any{"text": "hello"}},
			},
		}
		if ft.Parent != nil {
			p := domain.TurnRef{ID: ft.Parent.ID, Version: ft.Parent.Version}
			turn.Parent = &p
		}
		if _, err := repos.Turns().Append(ctx, turn); err != nil {
			t.Fatalf("append %s#%d: %v", ft.ID, ft.Version, err)
		}
	}

	// The conversation head follows the fixture's.
	if c.Input.Head != nil {
		head := domain.TurnRef{ID: c.Input.Head.ID, Version: c.Input.Head.Version}
		if _, err := repos.Conversations().SetHead(ctx, conv.ID, conv.Rev, &head); err != nil {
			t.Fatalf("set head: %v", err)
		}
		conv, err = repos.Conversations().Get(ctx, conv.ID)
		if err != nil {
			t.Fatalf("reload conversation: %v", err)
		}
	}

	// Head parity.
	if c.Expected.Head == nil {
		if conv.Head != nil {
			t.Errorf("head = %v, want nil", conv.Head)
		}
	} else {
		if conv.Head == nil {
			t.Fatalf("head = nil, want %s#%d", c.Expected.Head.ID, c.Expected.Head.Version)
		}
		if conv.Head.ID != c.Expected.Head.ID || conv.Head.Version != c.Expected.Head.Version {
			t.Errorf("head = %s#%d, want %s#%d", conv.Head.ID, conv.Head.Version,
				c.Expected.Head.ID, c.Expected.Head.Version)
		}
	}

	// Chain parity from the head. The fixture lists oldest-first; the
	// store walks newest-first, so the store's output is reversed to
	// compare.
	if conv.Head != nil {
		chain, err := repos.Turns().Chain(ctx, *conv.Head, store.ChainOptions{})
		if err != nil {
			t.Fatalf("chain: %v", err)
		}
		got := make([]string, 0, len(chain))
		for i := len(chain) - 1; i >= 0; i-- {
			got = append(got, refString(chain[i].Ref()))
		}
		if !equalStrings(got, c.Expected.Chain) {
			t.Errorf("chain = %v, want %v", got, c.Expected.Chain)
		}
	} else if len(c.Expected.Chain) != 0 {
		t.Errorf("expected a chain %v with no head", c.Expected.Chain)
	}

	// Node parity: parent, version list, children per version, actor.
	for id, want := range c.Expected.Nodes {
		versions, err := repos.Turns().Versions(ctx, id)
		if err != nil {
			t.Fatalf("versions of %s: %v", id, err)
		}
		gotVersions := make([]int, 0, len(versions))
		for _, v := range versions {
			gotVersions = append(gotVersions, v.Version)
		}
		if !equalInts(gotVersions, want.Versions) {
			t.Errorf("%s versions = %v, want %v", id, gotVersions, want.Versions)
		}

		// The newest version stands for the node, matching the fixture.
		newest := versions[len(versions)-1]
		turn, err := repos.Turns().Get(ctx, newest)
		if err != nil {
			t.Fatalf("get %s: %v", refString(newest), err)
		}
		if string(want.Parent) == "null" || len(want.Parent) == 0 {
			if turn.Parent != nil {
				t.Errorf("%s parent = %v, want nil", id, turn.Parent)
			}
		} else {
			var p domain.TurnRef
			if err := json.Unmarshal(want.Parent, &p); err != nil {
				t.Fatalf("decode expected parent: %v", err)
			}
			if turn.Parent == nil || *turn.Parent != p {
				t.Errorf("%s parent = %v, want %v", id, turn.Parent, p)
			}
		}
		if got, wantActor := turn.Actor.Kind, actors[want.Actor]; got != wantActor {
			t.Errorf("%s actor = %v, want %v", id, got, wantActor)
		}

		for versionStr, wantChildren := range want.Children {
			v, err := strconv.Atoi(versionStr)
			if err != nil {
				t.Fatalf("bad version key %q: %v", versionStr, err)
			}
			children, err := repos.Turns().Children(ctx, domain.TurnRef{ID: id, Version: v})
			if err != nil {
				t.Fatalf("children of %s#%d: %v", id, v, err)
			}
			// The port returns every version filed under the parent; the
			// fixture lists each child id once, at its newest version.
			gotChildren := collapseRefIDs(children)
			if !equalStrings(gotChildren, wantChildren) {
				t.Errorf("%s#%d children = %v, want %v", id, v, gotChildren, wantChildren)
			}
		}
	}

	// Query parity. The store answers chainFrom and branchInfo-shaped
	// questions directly; siblings at the tree level (turns sharing a
	// parent) is derived here because the port has no roots query.
	for _, q := range c.Expected.Queries {
		switch q.Op {
		case "chainFrom":
			// The fixture's chainFrom(id) is the chain anchored at the
			// newest version of that id. A missing id is an empty result
			// in the fixture and ErrNotFound in the store.
			versions, err := repos.Turns().Versions(ctx, q.Arg)
			if err != nil || len(versions) == 0 {
				if !errors.Is(err, store.ErrNotFound) && len(versions) != 0 {
					t.Fatalf("versions of %s: %v", q.Arg, err)
				}
				assertChainResult(t, q.Op, q.Arg, nil, q.Result)
				continue
			}
			newest := versions[len(versions)-1]
			chain, err := repos.Turns().Chain(ctx, newest, store.ChainOptions{})
			if err != nil {
				t.Fatalf("chain from %s: %v", q.Arg, err)
			}
			got := make([]string, 0, len(chain))
			for i := len(chain) - 1; i >= 0; i-- {
				got = append(got, refString(chain[i].Ref()))
			}
			assertChainResult(t, q.Op, q.Arg, got, q.Result)

		case "branchInfo":
			// branchInfo(id) is the version list plus where the head sits
			// in it. The fixture's shape carries currentIndex/hasPrev/
			// hasNext; the Go port exposes Versions and the head, and the
			// test derives the rest.
			versions, err := repos.Turns().Versions(ctx, q.Arg)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("versions of %s: %v", q.Arg, err)
			}
			var want struct {
				Versions     []int `json:"versions"`
				CurrentIndex int   `json:"currentIndex"`
			}
			if err := json.Unmarshal(q.Result, &want); err != nil {
				t.Fatalf("decode branchInfo result: %v", err)
			}
			gotVersions := make([]int, 0, len(versions))
			for _, v := range versions {
				gotVersions = append(gotVersions, v.Version)
			}
			if !equalInts(gotVersions, want.Versions) {
				t.Errorf("branchInfo(%s) versions = %v, want %v", q.Arg, gotVersions, want.Versions)
			}
			current := -1
			// currentIndex is the position of the version that sits on the
			// conversation head's chain — not the head itself. Walk the
			// chain and find which version of this id appears in it.
			if conv.Head != nil {
				chain, err := repos.Turns().Chain(ctx, *conv.Head, store.ChainOptions{})
				if err != nil {
					t.Fatalf("chain: %v", err)
				}
				onChain := map[int]bool{}
				for _, ct := range chain {
					if ct.ID == q.Arg {
						onChain[ct.Version] = true
					}
				}
				for i, rv := range versions {
					if onChain[rv.Version] {
						current = i
						break
					}
				}
			}
			if current != want.CurrentIndex {
				t.Errorf("branchInfo(%s) currentIndex = %d, want %d", q.Arg, current, want.CurrentIndex)
			}

		case "siblings":
			// siblings(id) in the fixture is every turn sharing the id's
			// parent — for roots, every root. The port answers Children
			// for a concrete parent; roots are derived by walking the
			// appended set, which the test still holds from the input.
			var want []string
			if err := json.Unmarshal(q.Result, &want); err != nil {
				t.Fatalf("decode siblings result: %v", err)
			}
			got := treeSiblings(t, repos, c.Input.Turns, q.Arg)
			if !equalStrings(got, want) {
				t.Errorf("siblings(%s) = %v, want %v", q.Arg, got, want)
			}

		default:
			t.Errorf("unhandled fixture query %q", q.Op)
		}
	}
}

// treeSiblings returns the fixture-level siblings of id: turns sharing
// its parent, roots included. It is derived from the store, not the
// input list, except that root detection needs the input's parentless
// set because the port cannot list roots.
func treeSiblings(t *testing.T, repos store.Repos, turns []fixtureTurn, id string) []string {
	t.Helper()
	ctx := t.Context()

	// Locate every version of id and use the newest for its parent.
	versions, err := repos.Turns().Versions(ctx, id)
	if err != nil || len(versions) == 0 {
		if len(versions) == 0 {
			return nil
		}
		t.Fatalf("versions of %s: %v", id, err)
	}
	newest := versions[len(versions)-1]
	turn, err := repos.Turns().Get(ctx, newest)
	if err != nil {
		t.Fatalf("get %s: %v", refString(newest), err)
	}

	var out []string
	if turn.Parent == nil {
		// Root siblings: every parentless fixture turn, each at version 1
		// (the fixture never versions a root twice).
		for _, ft := range turns {
			if ft.Parent == nil {
				out = append(out, ft.ID+"#1")
			}
		}
		return out
	}
	children, err := repos.Turns().Children(ctx, *turn.Parent)
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	// The fixture lists only the version that is on a live chain (the
	// newest that has children or is a head); the port returns every
	// version filed under the parent. Collapse to one ref per id: the
	// newest version of that id among the children.
	byID := map[string]domain.TurnRef{}
	var order []string
	for _, ch := range children {
		if _, seen := byID[ch.ID]; !seen {
			order = append(order, ch.ID)
		}
		if cur, ok := byID[ch.ID]; !ok || ch.Version > cur.Version {
			byID[ch.ID] = ch
		}
	}
	for _, cid := range order {
		out = append(out, refString(byID[cid]))
	}
	return out
}

func assertChainResult(t *testing.T, op, arg string, got []string, wantRaw json.RawMessage) {
	t.Helper()
	var want []string
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatalf("decode %s result: %v", op, err)
	}
	if got == nil {
		got = []string{}
	}
	if want == nil {
		want = []string{}
	}
	if !equalStrings(got, want) {
		t.Errorf("%s(%s) = %v, want %v", op, arg, got, want)
	}
}

func refString(r domain.TurnRef) string {
	return r.ID + "#" + strconv.Itoa(r.Version)
}

// collapseRefIDs reduces a child list to one ref per id — the newest
// version — in first-seen order, matching the fixture's children maps.
func collapseRefIDs(refs []domain.TurnRef) []string {
	byID := map[string]domain.TurnRef{}
	var order []string
	for _, r := range refs {
		if _, seen := byID[r.ID]; !seen {
			order = append(order, r.ID)
		}
		if cur, ok := byID[r.ID]; !ok || r.Version > cur.Version {
			byID[r.ID] = r
		}
	}
	out := make([]string, 0, len(order))
	for _, id := range order {
		out = append(out, id)
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

func equalInts(a, b []int) bool {
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

// Documented divergences from the TS fixture:
//
//   - Chains: the fixture lists oldest-first; Chain walks newest-first.
//   - chainFrom on a missing id: empty result in the fixture,
//     ErrNotFound from the store's Chain — the test maps the error to
//     the empty result.
//   - siblings: the fixture means "turns sharing a parent" (roots are
//     all parentless turns); the port's Branches means "versions of one
//     turn id". The golden derives the fixture's meaning from Children
//     plus the input's root set.
//   - branchInfo: the fixture's currentIndex/hasPrev/hasNext are derived
//     here from Versions and the conversation head; the port's
//     workspace.BranchInfo exposes Active + Siblings instead.
//   - Blocks: the fixture's block metadata (mode, topics, per-block
//     timestamps) is storage-level detail the Go turn tree does not
//     carry; the golden stores a minimal text block per turn.
