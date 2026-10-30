package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// testKeysetStability pages through a listing while a concurrent insert adds
// a record that sorts before the cursor. Keyset paging (docs/spike.md §15.7)
// must not duplicate or skip rows: the insert lands in a page the walk has
// already passed, so the walk sees exactly the rows that existed when it
// started, each once.
func testKeysetStability(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)

	const total = 40
	const pageSize = 7
	for i := 0; i < total; i++ {
		// Zero-padded ids sort lexicographically in insertion order, which
		// makes the expected page sequence exact.
		c := conversation(fmt.Sprintf("c%02d", i))
		if _, err := repos.Conversations().Create(ctx, c); err != nil {
			t.Fatalf("Create %s: %v", c.ID, err)
		}
	}

	repo := repos.Conversations()
	var seen []string
	cursor := ""
	pages := 0

	for {
		p, err := repo.List(ctx, store.ConversationFilter{}, store.PageRequest{
			Limit: pageSize, Cursor: cursor, Order: store.OldestFirst,
		})
		if err != nil {
			t.Fatalf("List page %d: %v", pages, err)
		}
		pages++
		for _, c := range p.Items {
			seen = append(seen, c.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor

		// Insert a record that sorts before the cursor mid-walk. Its id is
		// unique per page and always lexicographically below the current
		// cursor, so it belongs to a page the walk has already passed. It
		// must not shift the already-completed pages.
		early := conversation(fmt.Sprintf("a%02d", pages))
		if _, err := repos.Conversations().Create(ctx, early); err != nil {
			t.Fatalf("mid-walk Create: %v", err)
		}
	}

	// The walk saw the original total rows exactly once, in order.
	if len(seen) != total {
		t.Fatalf("paged %d rows, want %d", len(seen), total)
	}
	dupes := map[string]int{}
	for _, id := range seen {
		dupes[id]++
		if dupes[id] > 1 {
			t.Fatalf("row %s seen more than once (keyset paging is not stable)", id)
		}
	}
	for i := 0; i < total; i++ {
		want := fmt.Sprintf("c%02d", i)
		if seen[i] != want {
			t.Fatalf("row %d = %s, want %s", i, seen[i], want)
		}
	}

	// A limit of zero means "no limit" and returns everything in one page.
	// That is the original total plus one mid-walk insert per page that set a
	// cursor (the final page sets none, so it inserts nothing).
	inserted := pages - 1
	if inserted < 0 {
		inserted = 0
	}
	all, err := repo.List(ctx, store.ConversationFilter{}, store.PageRequest{})
	if err != nil {
		t.Fatalf("List unlimited: %v", err)
	}
	if len(all.Items) != total+inserted {
		t.Fatalf("unlimited list = %d rows, want %d", len(all.Items), total+inserted)
	}
}

func testReadYourWrites(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)

	err := st.Atomic(ctx, scope, store.TxOptions{}, func(ctx context.Context, tx store.Tx) error {
		if _, err := tx.Conversations().Create(ctx, conversation("c1")); err != nil {
			return fmt.Errorf("Create: %w", err)
		}
		// The write staged in this transaction must be visible to a read
		// inside the same transaction (docs/spike.md §15.5).
		got, err := tx.Conversations().Get(ctx, "c1")
		if err != nil {
			return fmt.Errorf("read-your-writes Get: %w", err)
		}
		if got.ID != "c1" {
			return fmt.Errorf("read-your-writes got %+v, want c1", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Atomic: %v", err)
	}

	// After commit the write is visible outside the transaction.
	repos, _ := st.Scoped(scope)
	if _, err := repos.Conversations().Get(ctx, "c1"); err != nil {
		t.Fatalf("Get after commit: %v", err)
	}
}

func testCommitHook(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)

	var hooks []string
	err := st.Atomic(ctx, scope, store.TxOptions{}, func(ctx context.Context, tx store.Tx) error {
		tx.AfterCommit(func() { hooks = append(hooks, "commit") })
		tx.AfterRollback(func() { hooks = append(hooks, "rollback") })
		_, err := tx.Conversations().Create(ctx, conversation("c1"))
		return err
	})
	if err != nil {
		t.Fatalf("Atomic: %v", err)
	}
	if len(hooks) != 1 || hooks[0] != "commit" {
		t.Fatalf("hooks after commit = %v, want [commit]", hooks)
	}
}

func testRollbackHidesWrite(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)

	var hooks []string
	boom := errors.New("boom")
	err := st.Atomic(ctx, scope, store.TxOptions{}, func(ctx context.Context, tx store.Tx) error {
		tx.AfterCommit(func() { hooks = append(hooks, "commit") })
		tx.AfterRollback(func() { hooks = append(hooks, "rollback") })
		if _, err := tx.Conversations().Create(ctx, conversation("c1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Atomic err = %v, want boom to propagate", err)
	}
	if len(hooks) != 1 || hooks[0] != "rollback" {
		t.Fatalf("hooks after rollback = %v, want [rollback]", hooks)
	}

	repos, _ := st.Scoped(scope)
	if _, err := repos.Conversations().Get(ctx, "c1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back write visible: Get err = %v, want ErrNotFound", err)
	}
}

func testCapabilities(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, _ := newStore(t, f)

	caps, err := st.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	// Every adapter the workspace ships must be serializable: the kernel
	// relies on it for the run claim protocol and for command idempotency.
	if caps.Transactions < store.TxSnapshot {
		t.Fatalf("Capabilities.Transactions = %v, want at least snapshot", caps.Transactions)
	}

	// Maintenance and Migrator must return something usable without a scope.
	if _, err := st.Maintenance(store.Scope{System: true}); err != nil {
		t.Fatalf("Maintenance: %v", err)
	}
	mig := st.Migrator()
	if v, err := mig.SchemaVersion(ctx); err != nil || v < 0 {
		t.Fatalf("SchemaVersion = %d, %v; want a non-negative version", v, err)
	}
	if _, err := mig.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

// ConcurrentAtomic exercises the run-claim protocol shape: many workers
// racing to claim the same queued runs. Exactly one worker may claim each
// run, which is what gives crash recovery its correctness.
func ConcurrentAtomic(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, _ := st.Scoped(scope)

	const runs = 20
	const workers = 8
	for i := 0; i < runs; i++ {
		r := domain.Run{
			ID:             fmt.Sprintf("r%02d", i),
			ConversationID: "c1",
			Status:         domain.RunQueued,
			StartedAt:      fixedTime,
		}
		if _, err := repos.Runs().Create(ctx, r); err != nil {
			t.Fatalf("Create run: %v", err)
		}
	}

	// Workers report into this slice; the main goroutine is the only caller
	// of t.Error*, because testing.T is not safe for concurrent use.
	type result struct {
		worker string
		runs   []domain.Run
		err    error
	}
	results := make([]result, workers)
	stale := fixedTime.Add(-time.Second)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			worker := fmt.Sprintf("w%d", w)
			got, err := repos.Runs().Claim(ctx, stale, runs, worker)
			results[w] = result{worker: worker, runs: got, err: err}
		}(w)
	}
	wg.Wait()

	claimedBy := map[string]string{}
	for _, res := range results {
		if res.err != nil {
			t.Fatalf("Claim(%s): %v", res.worker, res.err)
		}
		for _, r := range res.runs {
			if prev, ok := claimedBy[r.ID]; ok {
				t.Errorf("run %s claimed by both %s and %s", r.ID, prev, res.worker)
			}
			claimedBy[r.ID] = res.worker
		}
	}

	if len(claimedBy) != runs {
		t.Fatalf("claimed %d runs, want %d", len(claimedBy), runs)
	}
}
