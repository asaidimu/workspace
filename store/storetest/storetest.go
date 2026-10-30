// Package storetest is the conformance suite every store.Store adapter must
// pass. It is a set of test functions that take a factory returning a fresh,
// empty store, so the same assertions run against store/memory and
// store/anansistore without the suite knowing which backend it is on.
//
// The suite encodes the pinned semantics from the spikes (docs/spike.md):
// compare-and-swap on a stale version is ErrConflict, transactions give
// read-your-writes, a rollback hides the write, keyset paging is stable under
// concurrent inserts, and a topic "All" filter is an exact intersection.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// Factory returns a fresh, empty store. It is called once per test so tests
// never share state.
type Factory func(t testing.TB) store.Store

// fixedTime is the injected clock every test uses, so assertions on
// timestamps are exact rather than flaky.
var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// clock is a hand-cranked time source implementing domain.Clock.
type clock struct{ t time.Time }

func (c clock) Now() time.Time { return c.t }

// Run executes the whole suite against one adapter.
func Run(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Conversation", func(t *testing.T) {
		testConversationLifecycle(t, f)
		testConversationCAS(t, f)
		testConversationHead(t, f)
	})
	t.Run("Turn", func(t *testing.T) {
		testTurnTree(t, f)
		testChainOptions(t, f)
	})
	t.Run("Content", func(t *testing.T) {
		testContentVersions(t, f)
		testContentTombstone(t, f)
	})
	t.Run("Topics", func(t *testing.T) {
		testTopicFiltering(t, f)
	})
	t.Run("Paging", func(t *testing.T) {
		testKeysetStability(t, f)
	})
	t.Run("Tx", func(t *testing.T) {
		testReadYourWrites(t, f)
		testCommitHook(t, f)
		testRollbackHidesWrite(t, f)
	})
	t.Run("Run", func(t *testing.T) {
		testRunLifecycle(t, f)
	})
	t.Run("Capabilities", func(t *testing.T) {
		testCapabilities(t, f)
	})
	t.Run("ConcurrentAtomic", func(t *testing.T) {
		ConcurrentAtomic(t, f)
	})
}

// newStore returns a store with the injected clock and a handle to a
// system-scoped owner for convenience in tests.
func newStore(t testing.TB, f Factory) (store.Store, store.Scope) {
	t.Helper()
	st := f(t)
	t.Cleanup(func() {
		_ = st.Close(context.Background())
	})
	return st, store.Scope{Owner: "test"}
}

// conversation builds a conversation for the given id.
func conversation(id string) domain.Conversation {
	return domain.Conversation{
		ID:        id,
		Owner:     "test",
		Title:     id,
		Status:    domain.StatusActive,
		CreatedAt: fixedTime,
		UpdatedAt: fixedTime,
	}
}

func testConversationLifecycle(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, err := st.Scoped(scope)
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}

	created, err := repos.Conversations().Create(ctx, conversation("c1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "c1" || created.Owner != "test" || created.Rev != 1 {
		t.Fatalf("Create returned %+v, want id=c1 owner=test rev=1", created)
	}

	got, err := repos.Conversations().Get(ctx, "c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "c1" || got.Rev != 1 {
		t.Fatalf("Get returned %+v, want title=c1 rev=1", got)
	}

	if _, err := repos.Conversations().Create(ctx, conversation("c1")); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate Create err = %v, want ErrAlreadyExists", err)
	}

	if _, err := repos.Conversations().Get(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}

	if err := repos.Conversations().Delete(ctx, "c1", 1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repos.Conversations().Get(ctx, "c1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
}

func testConversationCAS(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)

	if _, err := repos.Conversations().Create(ctx, conversation("c1")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	title := "updated"
	patch := store.ConversationPatch{Title: &title}
	updated, err := repos.Conversations().Update(ctx, "c1", 1, patch)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Rev != 2 || updated.Title != "updated" {
		t.Fatalf("Update returned %+v, want rev=2 title=updated", updated)
	}

	// A stale expectedRev must be a conflict, and must not have written.
	if _, err := repos.Conversations().Update(ctx, "c1", 1, patch); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale Update err = %v, want ErrConflict", err)
	}
	got, _ := repos.Conversations().Get(ctx, "c1")
	if got.Rev != 2 || got.Title != "updated" {
		t.Fatalf("state after stale Update = %+v, want rev=2 title=updated", got)
	}

	// A delete with a stale rev is also a conflict.
	if err := repos.Conversations().Delete(ctx, "c1", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale Delete err = %v, want ErrConflict", err)
	}
}

func testConversationHead(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)

	if _, err := repos.Conversations().Create(ctx, conversation("c1")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	head := &domain.TurnRef{ID: "turn-1", Version: 1}
	withHead, err := repos.Conversations().SetHead(ctx, "c1", 1, head)
	if err != nil {
		t.Fatalf("SetHead: %v", err)
	}
	if withHead.Head == nil || *withHead.Head != *head || withHead.Rev != 2 {
		t.Fatalf("SetHead returned %+v, want head=turn-1/1 rev=2", withHead)
	}

	cleared, err := repos.Conversations().SetHead(ctx, "c1", 2, nil)
	if err != nil {
		t.Fatalf("SetHead nil: %v", err)
	}
	if cleared.Head != nil || cleared.Rev != 3 {
		t.Fatalf("SetHead nil returned %+v, want no head rev=3", cleared)
	}
}
