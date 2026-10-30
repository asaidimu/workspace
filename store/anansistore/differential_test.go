package anansistore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/memory"
)

// differential_test drives the same operation script against store/memory
// and store/anansistore and compares every result: sentinel classification
// for errors, normalized JSON for values, cursor emptiness (not cursor
// bytes — cursors are opaque per implementation) for pages. storetest
// covers the nine shared sections; this script reaches the repos storetest
// never calls (roles, blobs, receipts, settings) plus edge cases found while
// porting: nil-clears, dedupe-by-checksum, claim staleness, expiry.

var diffSentinels = []struct {
	name string
	err  error
}{
	{"NotFound", store.ErrNotFound},
	{"AlreadyExists", store.ErrAlreadyExists},
	{"Conflict", store.ErrConflict},
	{"Invalid", store.ErrInvalid},
	{"Busy", store.ErrBusy},
	{"TxClosed", store.ErrTxClosed},
	{"Unsupported", store.ErrUnsupported},
	{"Corrupt", store.ErrCorrupt},
	{"TooLarge", store.ErrTooLarge},
	{"Internal", store.ErrInternal},
}

func errKey(err error) string {
	if err == nil {
		return "nil"
	}
	for _, s := range diffSentinels {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	if errors.Is(err, context.Canceled) {
		return "ctx-canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "ctx-deadline"
	}
	return "other:" + err.Error()
}

// canon normalizes decoded JSON so nil and empty containers compare equal
// (memory can hold a nil slice where the adapter roundtrips an empty one,
// and vice versa) while preserving everything else.
func canon(v any) any {
	switch x := v.(type) {
	case []any:
		if len(x) == 0 {
			return nil
		}
		for i := range x {
			x[i] = canon(x[i])
		}
		return x
	case map[string]any:
		if len(x) == 0 {
			return nil
		}
		for k := range x {
			x[k] = canon(x[k])
		}
		return x
	}
	return v
}

func decodeCanon(t *testing.T, name, label string, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal %s result: %v", name, label, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("%s: decode %s result: %v", name, label, err)
	}
	return canon(out)
}

type diffPair struct {
	t   *testing.T
	ms  store.Repos
	as  store.Repos
	ms2 store.Repos
	as2 store.Repos
	stM store.Store
	stA store.Store
}

// step runs op against both stores and compares error + value.
func (d *diffPair) step(name string, op func(store.Repos) (any, error)) {
	d.t.Helper()
	mr, merr := op(d.ms)
	ar, aerr := op(d.as)
	if k1, k2 := errKey(merr), errKey(aerr); k1 != k2 {
		d.t.Errorf("%s: error mismatch: mem=%s anansi=%s", name, k1, k2)
		return
	}
	if merr != nil {
		return // value on error is unspecified; only parity of the error matters
	}
	mv := decodeCanon(d.t, name, "mem", mr)
	av := decodeCanon(d.t, name, "anansi", ar)
	if !reflect.DeepEqual(mv, av) {
		d.t.Errorf("%s: value mismatch\n mem=%#v\nana=%#v", name, mv, av)
	}
}

// step2 is step against the second owner's scope (scoping isolation).
func (d *diffPair) step2(name string, op func(store.Repos) (any, error)) {
	d.t.Helper()
	mr, merr := op(d.ms2)
	ar, aerr := op(d.as2)
	if k1, k2 := errKey(merr), errKey(aerr); k1 != k2 {
		d.t.Errorf("%s: error mismatch: mem=%s anansi=%s", name, k1, k2)
		return
	}
	if merr != nil {
		return
	}
	mv := decodeCanon(d.t, name, "mem", mr)
	av := decodeCanon(d.t, name, "anansi", ar)
	if !reflect.DeepEqual(mv, av) {
		d.t.Errorf("%s: value mismatch\n mem=%#v\nana=%#v", name, mv, av)
	}
}

// pair compares two one-shot operations that each target a specific store
// (they must not run twice — a second run would hit state the first created).
func (d *diffPair) pair(name string, mOp, aOp func() (any, error)) {
	d.t.Helper()
	mr, merr := mOp()
	ar, aerr := aOp()
	if k1, k2 := errKey(merr), errKey(aerr); k1 != k2 {
		d.t.Errorf("%s: error mismatch: mem=%s anansi=%s", name, k1, k2)
		return
	}
	if merr != nil {
		return
	}
	mv := decodeCanon(d.t, name, "mem", mr)
	av := decodeCanon(d.t, name, "anansi", ar)
	if !reflect.DeepEqual(mv, av) {
		d.t.Errorf("%s: value mismatch\n mem=%#v\nana=%#v", name, mv, av)
	}
}

// pageStep compares one keyset page and returns each side's own cursor for
// the next hop; only cursor emptiness must agree across implementations.
func pageStep[T any](d *diffPair, name, mCursor, aCursor string, op func(store.Repos, string) (store.Page[T], error)) (string, string) {
	d.t.Helper()
	mp, merr := op(d.ms, mCursor)
	ap, aerr := op(d.as, aCursor)
	if k1, k2 := errKey(merr), errKey(aerr); k1 != k2 {
		d.t.Errorf("%s: error mismatch: mem=%s anansi=%s", name, k1, k2)
		return "", ""
	}
	if merr != nil {
		return "", ""
	}
	mv := decodeCanon(d.t, name, "mem", mp.Items)
	av := decodeCanon(d.t, name, "anansi", ap.Items)
	if !reflect.DeepEqual(mv, av) {
		d.t.Errorf("%s: page items mismatch\n mem=%#v\nana=%#v", name, mv, av)
	}
	if (mp.NextCursor == "") != (ap.NextCursor == "") {
		d.t.Errorf("%s: cursor exhaustion mismatch: mem=%q anansi=%q", name, mp.NextCursor, ap.NextCursor)
	}
	return mp.NextCursor, ap.NextCursor
}

func TestDifferential(t *testing.T) {
	ctx := context.Background()
	scopeM := store.Scope{Owner: "alice"}
	mem := memory.New(memory.Options{Clock: clock{t: fixedTime}})
	p := newTestPersistence(t)
	ana, err := New(p, Options{Clock: clock{t: fixedTime}})
	if err != nil {
		t.Fatalf("anansistore.New: %v", err)
	}
	ms, err := mem.Scoped(scopeM)
	if err != nil {
		t.Fatal(err)
	}
	as, err := ana.Scoped(scopeM)
	if err != nil {
		t.Fatal(err)
	}
	ms2, err := mem.Scoped(store.Scope{Owner: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	as2, err := ana.Scoped(store.Scope{Owner: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	d := &diffPair{t: t, ms: ms, as: as, ms2: ms2, as2: as2, stM: mem, stA: ana}

	// --- scope validation ---
	d.pair("scope/invalid-owner",
		func() (any, error) { _, err := mem.Scoped(store.Scope{}); return errKey(err), nil },
		func() (any, error) { _, err := ana.Scoped(store.Scope{}); return errKey(err), nil })

	// --- conversations ---
	conv1 := domain.Conversation{ID: "c1", Owner: "alice", Title: "first", RoleID: "r1",
		Topics: []string{"alpha", "beta"}, Status: domain.StatusActive}
	d.step("conv/create", func(r store.Repos) (any, error) {
		return r.Conversations().Create(ctx, conv1)
	})
	d.step("conv/create-dup", func(r store.Repos) (any, error) {
		return r.Conversations().Create(ctx, conv1)
	})
	d.step("conv/create-bad-owner", func(r store.Repos) (any, error) {
		c := conv1
		c.ID = "cbad"
		c.Owner = ""
		return r.Conversations().Create(ctx, c)
	})
	d.step("conv/get", func(r store.Repos) (any, error) {
		return r.Conversations().Get(ctx, "c1")
	})
	d.step("conv/get-missing", func(r store.Repos) (any, error) {
		return r.Conversations().Get(ctx, "nope")
	})
	d.step("conv/create-cancelled-ctx", func(r store.Repos) (any, error) {
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		return r.Conversations().Create(cctx, conv1)
	})

	conv2 := domain.Conversation{ID: "c2", Owner: "alice", Title: "second", RoleID: "r1",
		Topics: []string{"beta"}, Status: domain.StatusArchived}
	d.step("conv/create-c2", func(r store.Repos) (any, error) {
		return r.Conversations().Create(ctx, conv2)
	})
	d.step2("conv/create-bob-c3", func(r store.Repos) (any, error) {
		return r.Conversations().Create(ctx, domain.Conversation{
			ID: "c3", Owner: "bob", Title: "bob's", Status: domain.StatusActive})
	})

	var mc, ac string
	mc, ac = pageStep(d, "conv/list-p1", mc, ac, func(r store.Repos, cur string) (store.Page[domain.Conversation], error) {
		return r.Conversations().List(ctx, store.ConversationFilter{}, store.PageRequest{Limit: 1, Cursor: cur, Order: store.OldestFirst})
	})
	mc, ac = pageStep(d, "conv/list-p2", mc, ac, func(r store.Repos, cur string) (store.Page[domain.Conversation], error) {
		return r.Conversations().List(ctx, store.ConversationFilter{}, store.PageRequest{Limit: 1, Cursor: cur, Order: store.OldestFirst})
	})
	_, _ = pageStep(d, "conv/list-p3", mc, ac, func(r store.Repos, cur string) (store.Page[domain.Conversation], error) {
		return r.Conversations().List(ctx, store.ConversationFilter{}, store.PageRequest{Limit: 5, Cursor: cur, Order: store.OldestFirst})
	})
	d.step("conv/list-scope-owns-only", func(r store.Repos) (any, error) {
		return r.Conversations().List(ctx, store.ConversationFilter{}, store.PageRequest{Limit: 10, Order: store.OldestFirst})
	})
	d.step("conv/list-filters", func(r store.Repos) (any, error) {
		return r.Conversations().List(ctx, store.ConversationFilter{
			IDs: []string{"c1", "c3"}, RoleID: "r1", Status: "active",
			Topics: store.TopicFilter{Any: []string{"alpha"}, All: []string{"beta"}},
		}, store.PageRequest{Limit: 10, Order: store.NewestFirst})
	})
	d.step("conv/update-patch", func(r store.Repos) (any, error) {
		c, err := r.Conversations().Get(ctx, "c1")
		if err != nil {
			return nil, err
		}
		title := "renamed"
		st := domain.StatusArchived
		return r.Conversations().Update(ctx, "c1", c.Rev, store.ConversationPatch{Title: &title, Status: &st})
	})
	d.step("conv/update-stale", func(r store.Repos) (any, error) {
		return r.Conversations().Update(ctx, "c1", 0, store.ConversationPatch{})
	})
	d.step("conv/update-missing", func(r store.Repos) (any, error) {
		return r.Conversations().Update(ctx, "gone", 1, store.ConversationPatch{})
	})
	d.step("conv/set-head", func(r store.Repos) (any, error) {
		c, err := r.Conversations().Get(ctx, "c1")
		if err != nil {
			return nil, err
		}
		return r.Conversations().SetHead(ctx, "c1", c.Rev, &domain.TurnRef{ID: "t1", Version: 1})
	})
	d.step("conv/set-head-nil", func(r store.Repos) (any, error) {
		c, err := r.Conversations().Get(ctx, "c1")
		if err != nil {
			return nil, err
		}
		return r.Conversations().SetHead(ctx, "c1", c.Rev, nil)
	})
	d.step("conv/set-head-stale", func(r store.Repos) (any, error) {
		return r.Conversations().SetHead(ctx, "c1", 999, &domain.TurnRef{ID: "t1", Version: 1})
	})

	// --- turns ---
	t1 := domain.Turn{ID: "t1", Version: 1, Owner: "alice",
		Actor: domain.Actor{Kind: domain.ActorUser, ID: "u1"},
		Blocks: []domain.Block{
			{Type: "text", Key: "say", Value: "hello", Raw: json.RawMessage(`"hello"`)},
			{Type: "tool", Key: "calc", Value: float64(42)},
			{Type: "json", Key: "cfg", Value: map[string]any{"n": float64(9007199254740993), "b": true}},
		},
		Usage:        domain.Usage{Model: "m", InputTokens: 3, OutputTokens: 4, CostMicros: 7, Currency: "KES"},
		FinishReason: "stop"}
	d.step("turn/append-v1", func(r store.Repos) (any, error) {
		return r.Turns().Append(ctx, t1)
	})
	d.step("turn/append-dup", func(r store.Repos) (any, error) {
		return r.Turns().Append(ctx, t1)
	})
	t2 := t1
	t2.Version = 2
	t2.Parent = &domain.TurnRef{ID: "t1", Version: 1}
	t2.Actor = domain.Actor{Kind: domain.ActorAgent, ID: "a1"}
	t2.Blocks = []domain.Block{{Type: "text", Key: "reply", Value: "world", Raw: json.RawMessage(`"world"`)}}
	t2.Usage = domain.Usage{}
	d.step("turn/append-v2", func(r store.Repos) (any, error) {
		return r.Turns().Append(ctx, t2)
	})
	t3 := t1
	t3.ID = "t2"
	t3.Parent = &domain.TurnRef{ID: "t1", Version: 2}
	d.step("turn/append-branch", func(r store.Repos) (any, error) {
		return r.Turns().Append(ctx, t3)
	})
	d.step("turn/append-bad-owner", func(r store.Repos) (any, error) {
		bad := t1
		bad.ID = "tbad"
		bad.Owner = "mallory"
		return r.Turns().Append(ctx, bad)
	})
	d.step("turn/append-cancelled-ctx", func(r store.Repos) (any, error) {
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		return r.Turns().Append(cctx, t1)
	})
	d.step("turn/get", func(r store.Repos) (any, error) {
		return r.Turns().Get(ctx, domain.TurnRef{ID: "t1", Version: 1})
	})
	d.step("turn/get-missing", func(r store.Repos) (any, error) {
		return r.Turns().Get(ctx, domain.TurnRef{ID: "t9", Version: 1})
	})
	d.step("turn/versions", func(r store.Repos) (any, error) {
		return r.Turns().Versions(ctx, "t1")
	})
	d.step("turn/versions-missing", func(r store.Repos) (any, error) {
		return r.Turns().Versions(ctx, "t9")
	})
	d.step("turn/children", func(r store.Repos) (any, error) {
		return r.Turns().Children(ctx, domain.TurnRef{ID: "t1", Version: 1})
	})
	d.step("turn/children-leaf", func(r store.Repos) (any, error) {
		return r.Turns().Children(ctx, domain.TurnRef{ID: "t2", Version: 1})
	})
	d.step("turn/chain-full", func(r store.Repos) (any, error) {
		return r.Turns().Chain(ctx, domain.TurnRef{ID: "t1", Version: 2}, store.ChainOptions{})
	})
	d.step("turn/chain-until", func(r store.Repos) (any, error) {
		return r.Turns().Chain(ctx, domain.TurnRef{ID: "t1", Version: 2},
			store.ChainOptions{Until: &domain.TurnRef{ID: "t1", Version: 1}})
	})
	d.step("turn/chain-limit", func(r store.Repos) (any, error) {
		return r.Turns().Chain(ctx, domain.TurnRef{ID: "t1", Version: 2}, store.ChainOptions{Limit: 1})
	})
	d.step("turn/chain-missing-head", func(r store.Repos) (any, error) {
		return r.Turns().Chain(ctx, domain.TurnRef{ID: "t9", Version: 1}, store.ChainOptions{})
	})
	tb := t1
	tb.ID = "t3"
	tb.Parent = &domain.TurnRef{ID: "ghost", Version: 1}
	d.step("turn/append-broken-parent", func(r store.Repos) (any, error) {
		return r.Turns().Append(ctx, tb)
	})
	d.step("turn/chain-broken-parent", func(r store.Repos) (any, error) {
		return r.Turns().Chain(ctx, domain.TurnRef{ID: "t3", Version: 1}, store.ChainOptions{})
	})

	// --- contents ---
	co1 := domain.Content{Key: "k1", Owner: "alice", Type: "doc", Mode: domain.ModeRead,
		Topics: []string{"alpha", "gamma"}, Value: json.RawMessage(`{"n":1}`),
		Metadata: map[string]string{"src": "test"}}
	d.step("content/put-v0", func(r store.Repos) (any, error) {
		return r.Contents().Put(ctx, co1, 0)
	})
	d.step("content/put-v0-dup", func(r store.Repos) (any, error) {
		return r.Contents().Put(ctx, co1, 0)
	})
	d.step("content/put-v2", func(r store.Repos) (any, error) {
		c := co1
		c.Value = json.RawMessage(`{"n":2}`)
		return r.Contents().Put(ctx, c, 1)
	})
	d.step("content/put-stale", func(r store.Repos) (any, error) {
		c := co1
		c.Value = json.RawMessage(`{"n":3}`)
		return r.Contents().Put(ctx, c, 1)
	})
	d.step("content/put-bad-base", func(r store.Repos) (any, error) {
		return r.Contents().Put(ctx, co1, -1)
	})
	d.step("content/put-missing-base", func(r store.Repos) (any, error) {
		return r.Contents().Put(ctx, co1, 7)
	})
	d.step("content/get-head", func(r store.Repos) (any, error) {
		return r.Contents().Get(ctx, domain.ContentRef{Key: "k1"})
	})
	d.step("content/get-v1", func(r store.Repos) (any, error) {
		return r.Contents().Get(ctx, domain.ContentRef{Key: "k1", Version: 1})
	})
	d.step("content/get-missing", func(r store.Repos) (any, error) {
		return r.Contents().Get(ctx, domain.ContentRef{Key: "k9", Version: 1})
	})
	d.step("content/versions", func(r store.Repos) (any, error) {
		return r.Contents().Versions(ctx, "k1")
	})
	d.step("content/list-heads", func(r store.Repos) (any, error) {
		return r.Contents().List(ctx, store.ContentFilter{}, store.PageRequest{Limit: 10, Order: store.OldestFirst})
	})
	d.step("content/list-all-versions", func(r store.Repos) (any, error) {
		return r.Contents().List(ctx, store.ContentFilter{AllVersions: true}, store.PageRequest{Limit: 10, Order: store.OldestFirst})
	})
	d.step("content/list-filtered", func(r store.Repos) (any, error) {
		return r.Contents().List(ctx, store.ContentFilter{
			Keys: []string{"k1", "k9"}, Types: []string{"doc"}, Modes: []string{string(domain.ModeRead)},
			Topics: store.TopicFilter{Any: []string{"gamma"}},
		}, store.PageRequest{Limit: 10, Order: store.NewestFirst})
	})
	d.step("content/tombstone", func(r store.Repos) (any, error) {
		return nil, r.Contents().Tombstone(ctx, "k1", 2)
	})
	d.step("content/tombstone-again", func(r store.Repos) (any, error) {
		return nil, r.Contents().Tombstone(ctx, "k1", 2)
	})
	d.step("content/tombstone-stale", func(r store.Repos) (any, error) {
		return nil, r.Contents().Tombstone(ctx, "k1", 1)
	})
	d.step("content/get-after-tombstone", func(r store.Repos) (any, error) {
		return r.Contents().Get(ctx, domain.ContentRef{Key: "k1"})
	})
	d.step("content/list-after-tombstone", func(r store.Repos) (any, error) {
		return r.Contents().List(ctx, store.ContentFilter{IncludeTombstoned: true, AllVersions: true},
			store.PageRequest{Limit: 10, Order: store.OldestFirst})
	})
	d.step("content/tombstone-missing", func(r store.Repos) (any, error) {
		return nil, r.Contents().Tombstone(ctx, "k9", 0)
	})

	// --- roles ---
	role1 := domain.Role{ID: "ro1", Owner: "alice", Name: "Helper", Persona: "kind",
		Topics: []string{"alpha"}, Constraints: json.RawMessage(`{"max":1}`)}
	d.step("role/put-create", func(r store.Repos) (any, error) {
		return r.Roles().Put(ctx, role1, 0)
	})
	d.step("role/put-create-dup", func(r store.Repos) (any, error) {
		return r.Roles().Put(ctx, role1, 0)
	})
	role2 := domain.Role{ID: "ro2", Owner: "alice", Name: "Other", Persona: "brief",
		PreferenceKeys: []domain.ContentRef{{Key: "k9", Version: 1}}}
	d.step("role/put-create-ro2", func(r store.Repos) (any, error) {
		return r.Roles().Put(ctx, role2, 0)
	})
	d.step("role/put-replace-clears", func(r store.Repos) (any, error) {
		r1 := role1
		r1.Persona = "kinder"
		r1.Constraints = nil
		r1.Topics = nil
		return r.Roles().Put(ctx, r1, 1)
	})
	d.step("role/put-stale", func(r store.Repos) (any, error) {
		return r.Roles().Put(ctx, role1, 0)
	})
	d.step("role/get", func(r store.Repos) (any, error) {
		return r.Roles().Get(ctx, "ro1")
	})
	d.step("role/get-missing", func(r store.Repos) (any, error) {
		return r.Roles().Get(ctx, "ro9")
	})
	var mrc, arc string
	mrc, arc = pageStep(d, "role/list-p1", mrc, arc, func(r store.Repos, cur string) (store.Page[domain.Role], error) {
		return r.Roles().List(ctx, store.RoleFilter{}, store.PageRequest{Limit: 1, Cursor: cur, Order: store.OldestFirst})
	})
	_, _ = pageStep(d, "role/list-p2", mrc, arc, func(r store.Repos, cur string) (store.Page[domain.Role], error) {
		return r.Roles().List(ctx, store.RoleFilter{}, store.PageRequest{Limit: 1, Cursor: cur, Order: store.OldestFirst})
	})
	d.step("role/list-filter", func(r store.Repos) (any, error) {
		return r.Roles().List(ctx, store.RoleFilter{IDs: []string{"ro2"}, Topics: store.TopicFilter{All: []string{"alpha"}}},
			store.PageRequest{Limit: 10, Order: store.NewestFirst})
	})
	d.step("role/delete-stale", func(r store.Repos) (any, error) {
		return nil, r.Roles().Delete(ctx, "ro1", 0)
	})
	d.step("role/delete", func(r store.Repos) (any, error) {
		rr, err := r.Roles().Get(ctx, "ro1")
		if err != nil {
			return nil, err
		}
		return nil, r.Roles().Delete(ctx, "ro1", rr.Rev)
	})
	d.step("role/delete-again", func(r store.Repos) (any, error) {
		return nil, r.Roles().Delete(ctx, "ro1", 1)
	})

	// --- blobs ---
	blob1 := domain.Blob{ID: "b1", Owner: "alice", Checksum: "sha256:aaa", Size: 10, MediaType: "text/plain", Name: "a.txt"}
	d.step("blob/register-new", func(r store.Repos) (any, error) {
		b, created, err := r.Blobs().Register(ctx, blob1)
		return struct {
			B domain.Blob
			C bool
		}{b, created}, err
	})
	d.step("blob/register-dup-checksum", func(r store.Repos) (any, error) {
		b, created, err := r.Blobs().Register(ctx, domain.Blob{ID: "b2", Owner: "alice", Checksum: "sha256:aaa", Size: 10})
		return struct {
			B domain.Blob
			C bool
		}{b, created}, err
	})
	d.step("blob/register-dup-id", func(r store.Repos) (any, error) {
		b, created, err := r.Blobs().Register(ctx, domain.Blob{ID: "b1", Owner: "alice", Checksum: "sha256:aaa", Size: 10})
		return struct {
			B domain.Blob
			C bool
		}{b, created}, err
	})
	d.step("blob/register-bad", func(r store.Repos) (any, error) {
		_, _, err := r.Blobs().Register(ctx, domain.Blob{ID: "bx", Owner: "alice"})
		return nil, err
	})
	d.step("blob/register-bad-scope", func(r store.Repos) (any, error) {
		_, _, err := r.Blobs().Register(ctx, domain.Blob{ID: "bx", Owner: "mallory", Checksum: "sha256:zzz"})
		return nil, err
	})
	d.step("blob/get", func(r store.Repos) (any, error) {
		return r.Blobs().Get(ctx, "b1")
	})
	d.step("blob/get-missing", func(r store.Repos) (any, error) {
		return r.Blobs().Get(ctx, "b9")
	})
	d.step("blob/get-by-checksum", func(r store.Repos) (any, error) {
		return r.Blobs().GetByChecksum(ctx, "sha256:aaa")
	})
	d.step("blob/get-by-checksum-missing", func(r store.Repos) (any, error) {
		return r.Blobs().GetByChecksum(ctx, "sha256:bbb")
	})
	d.step("blob/set-remote", func(r store.Repos) (any, error) {
		return nil, r.Blobs().SetRemote(ctx, "b1", "s3",
			domain.RemoteRef{ID: "s3://x", ExpiresAt: fixedTime.Add(24 * time.Hour)})
	})
	d.step("blob/set-remote-missing", func(r store.Repos) (any, error) {
		return nil, r.Blobs().SetRemote(ctx, "b9", "s3", domain.RemoteRef{ID: "s3://x"})
	})
	holder := domain.EntityRef{Kind: domain.EntityTurn, ID: "t1", Version: 1}
	d.step("blob/add-ref", func(r store.Repos) (any, error) {
		return nil, r.Blobs().AddRef(ctx, "b1", holder)
	})
	d.step("blob/add-ref-again", func(r store.Repos) (any, error) {
		return nil, r.Blobs().AddRef(ctx, "b1", holder)
	})
	d.step("blob/add-ref-missing", func(r store.Repos) (any, error) {
		return nil, r.Blobs().AddRef(ctx, "b9", holder)
	})
	d.step("blob/refs", func(r store.Repos) (any, error) {
		return r.Blobs().Refs(ctx, "b1")
	})
	d.step("blob/refs-missing", func(r store.Repos) (any, error) {
		return r.Blobs().Refs(ctx, "b9")
	})
	d.step("blob/remove-ref", func(r store.Repos) (any, error) {
		return nil, r.Blobs().RemoveRef(ctx, "b1", holder)
	})
	d.step("blob/remove-ref-again", func(r store.Repos) (any, error) {
		return nil, r.Blobs().RemoveRef(ctx, "b1", holder)
	})
	d.step("blob/orphans-none-yet", func(r store.Repos) (any, error) {
		return r.Blobs().Orphans(ctx, fixedTime, 10)
	})
	d.step("blob/orphans-all", func(r store.Repos) (any, error) {
		return r.Blobs().Orphans(ctx, fixedTime.Add(time.Second), 10)
	})
	d.step("blob/orphans-limit0", func(r store.Repos) (any, error) {
		return r.Blobs().Orphans(ctx, fixedTime.Add(time.Second), 0)
	})
	d.step("blob/delete", func(r store.Repos) (any, error) {
		return nil, r.Blobs().Delete(ctx, "b1")
	})
	d.step("blob/delete-again", func(r store.Repos) (any, error) {
		return nil, r.Blobs().Delete(ctx, "b1")
	})
	d.step("blob/get-after-delete", func(r store.Repos) (any, error) {
		return r.Blobs().Get(ctx, "b1")
	})
	d.step("blob/refs-after-delete", func(r store.Repos) (any, error) {
		return r.Blobs().Refs(ctx, "b1")
	})

	// --- runs ---
	run1 := domain.Run{ID: "rn1", ConversationID: "c1", State: json.RawMessage(`{"s":1}`)}
	d.step("run/create", func(r store.Repos) (any, error) {
		return r.Runs().Create(ctx, run1)
	})
	d.step("run/create-dup", func(r store.Repos) (any, error) {
		return r.Runs().Create(ctx, run1)
	})
	d.step("run/create-bad", func(r store.Repos) (any, error) {
		return r.Runs().Create(ctx, domain.Run{ID: "rnx"})
	})
	d.step("run/get", func(r store.Repos) (any, error) {
		return r.Runs().Get(ctx, "rn1")
	})
	d.step("run/get-missing", func(r store.Repos) (any, error) {
		return r.Runs().Get(ctx, "rn9")
	})
	d.step("run/checkpoint", func(r store.Repos) (any, error) {
		return r.Runs().Checkpoint(ctx, "rn1", 0, json.RawMessage(`{"s":2}`))
	})
	d.step("run/checkpoint-stale", func(r store.Repos) (any, error) {
		return r.Runs().Checkpoint(ctx, "rn1", 0, nil)
	})
	d.step("run/checkpoint-missing", func(r store.Repos) (any, error) {
		return r.Runs().Checkpoint(ctx, "rn9", 0, nil)
	})
	d.step("run/set-status", func(r store.Repos) (any, error) {
		return r.Runs().SetStatus(ctx, "rn1", 1, domain.RunWaiting)
	})
	d.step("run/set-status-terminal", func(r store.Repos) (any, error) {
		return r.Runs().SetStatus(ctx, "rn1", 1, domain.RunSucceeded)
	})
	d.step("run/set-status-back", func(r store.Repos) (any, error) {
		return r.Runs().SetStatus(ctx, "rn1", 1, domain.RunRunning)
	})
	d.step("run/set-status-bad-step", func(r store.Repos) (any, error) {
		return r.Runs().SetStatus(ctx, "rn1", 0, domain.RunWaiting)
	})
	d.step("run/heartbeat", func(r store.Repos) (any, error) {
		return nil, r.Runs().Heartbeat(ctx, "rn1", "w1")
	})
	d.step("run/heartbeat-missing", func(r store.Repos) (any, error) {
		return nil, r.Runs().Heartbeat(ctx, "rn9", "w1")
	})
	d.step("run/list-active", func(r store.Repos) (any, error) {
		return r.Runs().ListActive(ctx, "c1")
	})
	// claim: rn1 is running with heartbeat=fixedTime; stale cutoff after that
	// reclaims it; a fresh queued run joins it; finished/absent never appear.
	d.step("run/create-queued", func(r store.Repos) (any, error) {
		return r.Runs().Create(ctx, domain.Run{ID: "rn2", ConversationID: "c1"})
	})
	d.step("run/claim-bad-args", func(r store.Repos) (any, error) {
		return r.Runs().Claim(ctx, fixedTime, 1, "")
	})
	d.step("run/claim", func(r store.Repos) (any, error) {
		return r.Runs().Claim(ctx, fixedTime.Add(time.Second), 5, "w1")
	})
	d.step("run/claim-again-none", func(r store.Repos) (any, error) {
		return r.Runs().Claim(ctx, fixedTime.Add(time.Second), 5, "w2")
	})
	d.step("run/finish-invalid", func(r store.Repos) (any, error) {
		return r.Runs().Finish(ctx, "rn1", domain.RunRunning, "")
	})
	d.step("run/finish", func(r store.Repos) (any, error) {
		return r.Runs().Finish(ctx, "rn1", domain.RunFailed, "boom")
	})
	d.step("run/set-status-after-terminal", func(r store.Repos) (any, error) {
		return r.Runs().SetStatus(ctx, "rn1", 2, domain.RunWaiting)
	})
	d.step("run/claim-after-terminal", func(r store.Repos) (any, error) {
		return r.Runs().Claim(ctx, fixedTime.Add(time.Hour), 5, "w3")
	})

	// --- receipts ---
	d.step("receipt/reserve-new", func(r store.Repos) (any, error) {
		rec, created, err := r.Receipts().Reserve(ctx, "idem1", time.Hour)
		return struct {
			R domain.Receipt
			C bool
		}{rec, created}, err
	})
	d.step("receipt/reserve-existing", func(r store.Repos) (any, error) {
		rec, created, err := r.Receipts().Reserve(ctx, "idem1", time.Hour)
		return struct {
			R domain.Receipt
			C bool
		}{rec, created}, err
	})
	d.step("receipt/complete", func(r store.Repos) (any, error) {
		return nil, r.Receipts().Complete(ctx, "idem1", json.RawMessage(`{"ok":true}`))
	})
	d.step("receipt/reserve-completed", func(r store.Repos) (any, error) {
		rec, created, err := r.Receipts().Reserve(ctx, "idem1", time.Hour)
		return struct {
			R domain.Receipt
			C bool
		}{rec, created}, err
	})
	d.step("receipt/complete-missing", func(r store.Repos) (any, error) {
		return nil, r.Receipts().Complete(ctx, "nope", nil)
	})
	d.step("receipt/reserve-expired", func(r store.Repos) (any, error) {
		rec, created, err := r.Receipts().Reserve(ctx, "idem2", -time.Second)
		return struct {
			R domain.Receipt
			C bool
		}{rec, created}, err
	})
	d.step("receipt/reserve-expired-again", func(r store.Repos) (any, error) {
		rec, created, err := r.Receipts().Reserve(ctx, "idem2", -time.Second)
		return struct {
			R domain.Receipt
			C bool
		}{rec, created}, err
	})
	d.step("receipt/complete-expired", func(r store.Repos) (any, error) {
		_, _, _ = r.Receipts().Reserve(ctx, "idem3", -time.Second)
		return nil, r.Receipts().Complete(ctx, "idem3", nil)
	})

	// --- settings (owner scope) ---
	d.step("settings/get-missing", func(r store.Repos) (any, error) {
		return r.Settings().Get(ctx)
	})
	d.step("settings/put-create", func(r store.Repos) (any, error) {
		return r.Settings().Put(ctx, domain.Settings{Language: "en"}, 0)
	})
	d.step("settings/put-create-again", func(r store.Repos) (any, error) {
		return r.Settings().Put(ctx, domain.Settings{Language: "fr"}, 0)
	})
	d.step("settings/put-replace", func(r store.Repos) (any, error) {
		s, err := r.Settings().Get(ctx)
		if err != nil {
			return nil, err
		}
		return r.Settings().Put(ctx, domain.Settings{Language: "sw", DefaultRoleID: "ro2",
			GlobalPreferenceKeys: []domain.ContentRef{{Key: "k9"}}}, s.Rev)
	})
	d.step("settings/put-stale", func(r store.Repos) (any, error) {
		return r.Settings().Put(ctx, domain.Settings{Language: "xx"}, 0)
	})
	d.step("settings/get", func(r store.Repos) (any, error) {
		return r.Settings().Get(ctx)
	})

	// --- settings (system scope) ---
	d.step2("settings/system-is-separate", func(r store.Repos) (any, error) {
		return r.Settings().Get(ctx) // bob scope: still missing
	})

	// --- maintenance, migrator, capabilities ---
	sweep := func(st store.Store) func() (any, error) {
		return func() (any, error) {
			m, err := st.Maintenance(scopeM)
			if err != nil {
				return nil, err
			}
			opts := store.SweepOptions{Grace: time.Hour, Limit: 10, DryRun: false}
			t1, err1 := m.SweepTurns(ctx, opts)
			b1, err2 := m.SweepBlobs(ctx, opts)
			r1, err3 := m.SweepReceipts(ctx, fixedTime)
			return []any{t1, b1, r1, errKey(err1), errKey(err2), errKey(err3)}, nil
		}
	}
	d.pair("maint/sweeps-noop", sweep(mem), sweep(ana))
	mrep, merr := mem.Migrator().Migrate(ctx)
	arep, aerr := ana.Migrator().Migrate(ctx)
	if errKey(merr) != errKey(aerr) || !reflect.DeepEqual(mrep, arep) {
		t.Errorf("migrator: mem=(%#v,%s) ana=(%#v,%s)", mrep, errKey(merr), arep, errKey(aerr))
	}
	mv, _ := mem.Migrator().SchemaVersion(ctx)
	av, _ := ana.Migrator().SchemaVersion(ctx)
	if mv != av {
		t.Errorf("schema version: mem=%d ana=%d", mv, av)
	}
	mcaps, merr := mem.Capabilities(ctx)
	acaps, aerr := ana.Capabilities(ctx)
	if errKey(merr) != errKey(aerr) {
		t.Errorf("capabilities err: mem=%s ana=%s", errKey(merr), errKey(aerr))
	}
	// RecursiveQueries is the one documented capability difference: the
	// memory chain walk is recursive by construction; the adapter walks
	// parent links iteratively and reports false rather than lying.
	if mcaps.RecursiveQueries != true || acaps.RecursiveQueries != false {
		t.Errorf("capabilities RecursiveQueries: mem=%v ana=%v (expected true/false)", mcaps.RecursiveQueries, acaps.RecursiveQueries)
	}
	maps := reflect.ValueOf(mcaps)
	avv := reflect.ValueOf(acaps)
	for i := 0; i < maps.NumField(); i++ {
		if maps.Type().Field(i).Name == "RecursiveQueries" {
			continue
		}
		if !reflect.DeepEqual(maps.Field(i).Interface(), avv.Field(i).Interface()) {
			t.Errorf("capabilities %s: mem=%v ana=%v", maps.Type().Field(i).Name,
				maps.Field(i).Interface(), avv.Field(i).Interface())
		}
	}

	// --- atomic ---
	d.pair("tx/commit-hooks",
		func() (any, error) {
			commits, rollbacks := 0, 0
			err := mem.Atomic(ctx, scopeM, store.TxOptions{}, func(tctx context.Context, tx store.Tx) error {
				tx.AfterCommit(func() { commits++ })
				tx.AfterRollback(func() { rollbacks++ })
				_, err := tx.Conversations().Create(tctx, domain.Conversation{ID: "tx1", Owner: "alice", Status: domain.StatusActive})
				return err
			})
			seen, gerr := ms.Conversations().Get(ctx, "tx1")
			return []any{errKey(err), commits, rollbacks, errKey(gerr), seen.ID}, gerr
		},
		func() (any, error) {
			commits, rollbacks := 0, 0
			err := ana.Atomic(ctx, scopeM, store.TxOptions{}, func(tctx context.Context, tx store.Tx) error {
				tx.AfterCommit(func() { commits++ })
				tx.AfterRollback(func() { rollbacks++ })
				_, err := tx.Conversations().Create(tctx, domain.Conversation{ID: "tx1", Owner: "alice", Status: domain.StatusActive})
				return err
			})
			seen, gerr := as.Conversations().Get(ctx, "tx1")
			return []any{errKey(err), commits, rollbacks, errKey(gerr), seen.ID}, gerr
		})
	d.pair("tx/rollback-hooks",
		func() (any, error) {
			commits, rollbacks := 0, 0
			err := mem.Atomic(ctx, scopeM, store.TxOptions{}, func(tctx context.Context, tx store.Tx) error {
				tx.AfterCommit(func() { commits++ })
				tx.AfterRollback(func() { rollbacks++ })
				_, err := tx.Conversations().Create(tctx, domain.Conversation{ID: "tx2", Owner: "alice", Status: domain.StatusActive})
				if err != nil {
					return err
				}
				return errors.New("abort")
			})
			_, gerr := ms.Conversations().Get(ctx, "tx2")
			return []any{errKey(err), err != nil && err.Error() == "abort", commits, rollbacks, errKey(gerr)}, nil
		},
		func() (any, error) {
			commits, rollbacks := 0, 0
			err := ana.Atomic(ctx, scopeM, store.TxOptions{}, func(tctx context.Context, tx store.Tx) error {
				tx.AfterCommit(func() { commits++ })
				tx.AfterRollback(func() { rollbacks++ })
				_, err := tx.Conversations().Create(tctx, domain.Conversation{ID: "tx2", Owner: "alice", Status: domain.StatusActive})
				if err != nil {
					return err
				}
				return errors.New("abort")
			})
			_, gerr := as.Conversations().Get(ctx, "tx2")
			return []any{errKey(err), err != nil && err.Error() == "abort", commits, rollbacks, errKey(gerr)}, nil
		})
	d.pair("tx/read-only-ok",
		func() (any, error) {
			commits := 0
			err := mem.Atomic(ctx, scopeM, store.TxOptions{ReadOnly: true}, func(tctx context.Context, tx store.Tx) error {
				tx.AfterCommit(func() { commits++ })
				_, err := tx.Conversations().Get(tctx, "c2")
				return err
			})
			return []any{errKey(err), commits}, nil
		},
		func() (any, error) {
			commits := 0
			err := ana.Atomic(ctx, scopeM, store.TxOptions{ReadOnly: true}, func(tctx context.Context, tx store.Tx) error {
				tx.AfterCommit(func() { commits++ })
				_, err := tx.Conversations().Get(tctx, "c2")
				return err
			})
			return []any{errKey(err), commits}, nil
		})
	d.pair("tx/read-only-write",
		func() (any, error) {
			err := mem.Atomic(ctx, scopeM, store.TxOptions{ReadOnly: true}, func(tctx context.Context, tx store.Tx) error {
				_, werr := tx.Conversations().Create(tctx, domain.Conversation{ID: "txro", Owner: "alice", Status: domain.StatusActive})
				return werr
			})
			_, gerr := ms.Conversations().Get(ctx, "txro")
			return []any{errKey(err), err != nil, errKey(gerr)}, nil
		},
		func() (any, error) {
			err := ana.Atomic(ctx, scopeM, store.TxOptions{ReadOnly: true}, func(tctx context.Context, tx store.Tx) error {
				_, werr := tx.Conversations().Create(tctx, domain.Conversation{ID: "txro", Owner: "alice", Status: domain.StatusActive})
				return werr
			})
			_, gerr := as.Conversations().Get(ctx, "txro")
			return []any{errKey(err), err != nil, errKey(gerr)}, nil
		})
	d.pair("tx/closed-tx-reuse",
		func() (any, error) {
			var captured store.Tx
			_ = mem.Atomic(ctx, scopeM, store.TxOptions{}, func(tctx context.Context, tx store.Tx) error {
				captured = tx
				return nil
			})
			_, err := captured.Conversations().Get(ctx, "c1")
			return errKey(err), nil
		},
		func() (any, error) {
			var captured store.Tx
			_ = ana.Atomic(ctx, scopeM, store.TxOptions{}, func(tctx context.Context, tx store.Tx) error {
				captured = tx
				return nil
			})
			_, err := captured.Conversations().Get(ctx, "c1")
			return errKey(err), nil
		})
	d.pair("tx/cancelled-ctx",
		func() (any, error) {
			cctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := mem.Atomic(cctx, scopeM, store.TxOptions{}, func(context.Context, store.Tx) error { return nil })
			return errKey(err), nil
		},
		func() (any, error) {
			cctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := ana.Atomic(cctx, scopeM, store.TxOptions{}, func(context.Context, store.Tx) error { return nil })
			return errKey(err), nil
		})

	// --- final conversation deletion ---
	d.step("conv/delete-stale", func(r store.Repos) (any, error) {
		return nil, r.Conversations().Delete(ctx, "c1", 0)
	})
	d.step("conv/delete", func(r store.Repos) (any, error) {
		c, err := r.Conversations().Get(ctx, "c1")
		if err != nil {
			return nil, err
		}
		return nil, r.Conversations().Delete(ctx, "c1", c.Rev)
	})
	d.step("conv/delete-again", func(r store.Repos) (any, error) {
		return nil, r.Conversations().Delete(ctx, "c1", 1)
	})
	d.step("conv/get-after-delete", func(r store.Repos) (any, error) {
		return r.Conversations().Get(ctx, "c1")
	})
}
