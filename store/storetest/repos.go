package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// turn builds a turn for the given id and version, optionally parented.
func turn(id string, version int, parent *domain.TurnRef) domain.Turn {
	return domain.Turn{
		ID:        id,
		Version:   version,
		Owner:     "test",
		Parent:    parent,
		Actor:     domain.Actor{Kind: domain.ActorUser, ID: "u1"},
		CreatedAt: fixedTime,
	}
}

func testTurnTree(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)
	tr := repos.Turns()

	if _, err := tr.Append(ctx, turn("t1", 1, nil)); err != nil {
		t.Fatalf("Append t1: %v", err)
	}
	p1 := &domain.TurnRef{ID: "t1", Version: 1}
	if _, err := tr.Append(ctx, turn("t2", 1, p1)); err != nil {
		t.Fatalf("Append t2: %v", err)
	}

	// The same (id, version) twice is an already-exists, not a new record.
	if _, err := tr.Append(ctx, turn("t2", 1, p1)); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate Append err = %v, want ErrAlreadyExists", err)
	}

	// A different version of the same id is a distinct, legal record.
	if _, err := tr.Append(ctx, turn("t2", 2, p1)); err != nil {
		t.Fatalf("Append t2 v2: %v", err)
	}

	vers, err := tr.Versions(ctx, "t2")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(vers) != 2 || vers[0].Version != 1 || vers[1].Version != 2 {
		t.Fatalf("Versions = %v, want [1 2]", vers)
	}

	kids, err := tr.Children(ctx, *p1)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(kids) != 2 {
		t.Fatalf("Children = %v, want 2 children of t1/1", kids)
	}
}

func testChainOptions(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)
	tr := repos.Turns()

	// Build a linear chain: t1 <- t2 <- t3 <- t4.
	if _, err := tr.Append(ctx, turn("t1", 1, nil)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	p1 := &domain.TurnRef{ID: "t1", Version: 1}
	if _, err := tr.Append(ctx, turn("t2", 1, p1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	p2 := &domain.TurnRef{ID: "t2", Version: 1}
	if _, err := tr.Append(ctx, turn("t3", 1, p2)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	p3 := &domain.TurnRef{ID: "t3", Version: 1}
	if _, err := tr.Append(ctx, turn("t4", 1, p3)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	head := domain.TurnRef{ID: "t4", Version: 1}
	full, err := tr.Chain(ctx, head, store.ChainOptions{})
	if err != nil {
		t.Fatalf("Chain full: %v", err)
	}
	if len(full) != 4 || full[0].ID != "t4" || full[3].ID != "t1" {
		t.Fatalf("Chain full = %d turns starting %s, want 4 starting t4", len(full), full[0].ID)
	}

	limited, err := tr.Chain(ctx, head, store.ChainOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Chain limit: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != "t4" || limited[1].ID != "t3" {
		t.Fatalf("Chain limit = %v, want [t4 t3]", limited)
	}

	until := domain.TurnRef{ID: "t3", Version: 1}
	part, err := tr.Chain(ctx, head, store.ChainOptions{Until: &until})
	if err != nil {
		t.Fatalf("Chain until: %v", err)
	}
	if len(part) != 2 || part[0].ID != "t4" || part[1].ID != "t3" {
		t.Fatalf("Chain until = %v, want [t4 t3]", part)
	}

	// A head that does not exist is a not-found rather than an empty chain.
	if _, err := tr.Chain(ctx, domain.TurnRef{ID: "nope", Version: 1}, store.ChainOptions{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Chain missing head err = %v, want ErrNotFound", err)
	}
}

func testContentVersions(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)
	cr := repos.Contents()

	base := domain.Content{
		Key: "doc", Owner: "test", Type: "text",
		Mode: domain.ModeRead, CreatedAt: fixedTime,
	}

	v1, err := cr.Put(ctx, base, 0)
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("Put v1 version = %d, want 1", v1.Version)
	}

	// Creating over an existing key is a conflict, not a silent overwrite.
	if _, err := cr.Put(ctx, base, 0); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("re-create err = %v, want ErrAlreadyExists", err)
	}

	// A stale base version is a CAS miss.
	base.Value = []byte(`{"n":2}`)
	if _, err := cr.Put(ctx, base, 5); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale Put err = %v, want ErrConflict", err)
	}

	// The correct base version advances to v2.
	base.Value = []byte(`{"n":2}`)
	v2, err := cr.Put(ctx, base, 1)
	if err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("Put v2 version = %d, want 2", v2.Version)
	}

	vers, err := cr.Versions(ctx, "doc")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(vers) != 2 || vers[0].Version != 1 || vers[1].Version != 2 {
		t.Fatalf("Versions = %v, want [1 2]", vers)
	}

	// A version-0 read is the latest non-tombstoned version.
	latest, err := cr.Get(ctx, domain.ContentRef{Key: "doc", Version: 0})
	if err != nil {
		t.Fatalf("Get latest: %v", err)
	}
	if latest.Version != 2 {
		t.Fatalf("Get latest version = %d, want 2", latest.Version)
	}

	// A specific version reads that exact version.
	specific, err := cr.Get(ctx, domain.ContentRef{Key: "doc", Version: 1})
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	if specific.Version != 1 {
		t.Fatalf("Get v1 version = %d, want 1", specific.Version)
	}
}

func testContentTombstone(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)
	cr := repos.Contents()

	base := domain.Content{
		Key: "doc", Owner: "test", Type: "text",
		Mode: domain.ModeRead, CreatedAt: fixedTime,
	}
	if _, err := cr.Put(ctx, base, 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Tombstoning an unknown key is a not-found.
	if err := cr.Tombstone(ctx, "missing", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Tombstone missing err = %v, want ErrNotFound", err)
	}

	// Tombstoning with a stale base version is a conflict.
	if err := cr.Tombstone(ctx, "doc", 7); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale Tombstone err = %v, want ErrConflict", err)
	}

	if err := cr.Tombstone(ctx, "doc", 1); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if _, err := cr.Get(ctx, domain.ContentRef{Key: "doc", Version: 0}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after Tombstone err = %v, want ErrNotFound", err)
	}

	// The version history survives the tombstone, so a rollback is possible.
	vers, err := cr.Versions(ctx, "doc")
	if err != nil {
		t.Fatalf("Versions after tombstone: %v", err)
	}
	if len(vers) != 1 {
		t.Fatalf("Versions after tombstone = %v, want the 1 existing version", vers)
	}
}

func testTopicFiltering(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)

	mk := func(id string, topics []string) domain.Conversation {
		c := conversation(id)
		c.Topics = topics
		return c
	}
	for _, c := range []domain.Conversation{
		mk("c1", []string{"a", "b"}),
		mk("c2", []string{"a", "c"}),
		mk("c3", []string{"b", "c"}),
		mk("c4", []string{"a", "b", "c"}),
		mk("c5", nil),
	} {
		if _, err := repos.Conversations().Create(ctx, c); err != nil {
			t.Fatalf("Create %s: %v", c.ID, err)
		}
	}
	repo := repos.Conversations()

	// Any is a union.
	page, err := repo.List(ctx, store.ConversationFilter{
		Topics: store.TopicFilter{Any: []string{"a"}},
	}, store.PageRequest{})
	if err != nil {
		t.Fatalf("List Any a: %v", err)
	}
	if ids := idsOf(page.Items); len(ids) != 3 {
		t.Fatalf("Any a = %v, want 3", ids)
	}

	// All is an exact intersection.
	page, err = repo.List(ctx, store.ConversationFilter{
		Topics: store.TopicFilter{All: []string{"a", "b"}},
	}, store.PageRequest{})
	if err != nil {
		t.Fatalf("List All a,b: %v", err)
	}
	if ids := idsOf(page.Items); len(ids) != 2 {
		t.Fatalf("All a,b = %v, want 2", ids)
	}

	// Any and All together are ANDed.
	page, err = repo.List(ctx, store.ConversationFilter{
		Topics: store.TopicFilter{Any: []string{"a"}, All: []string{"b"}},
	}, store.PageRequest{})
	if err != nil {
		t.Fatalf("List Any a All b: %v", err)
	}
	if ids := idsOf(page.Items); len(ids) != 2 {
		t.Fatalf("Any a AND All b = %v, want 2", ids)
	}

	// An empty filter matches everything, including the topic-less record.
	page, err = repo.List(ctx, store.ConversationFilter{}, store.PageRequest{})
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("empty filter matched %d, want 5", len(page.Items))
	}
}

func idsOf(cs []domain.Conversation) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}
