package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// testRunLifecycle covers the run-claim protocol the agent loop's crash
// recovery depends on: the step compare-and-swap, the queued/running/waiting
// transitions section 11 requires, and the fact that a parked run is neither
// claimable nor listed as active.
func testRunLifecycle(t *testing.T, f Factory) {
	t.Helper()
	ctx := context.Background()
	st, scope := newStore(t, f)
	repos, err := st.Scoped(scope)
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	runs := repos.Runs()

	run, err := runs.Create(ctx, domain.Run{ID: "r1", ConversationID: "c1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if run.Status != domain.RunQueued {
		t.Errorf("status = %q, want queued", run.Status)
	}

	running, err := runs.SetStatus(ctx, "r1", 0, domain.RunRunning)
	if err != nil {
		t.Fatalf("SetStatus running: %v", err)
	}
	if running.Status != domain.RunRunning {
		t.Errorf("status = %q, want running", running.Status)
	}

	// A stale step guard is a conflict: two workers cannot both decide.
	if _, err := runs.SetStatus(ctx, "r1", 7, domain.RunWaiting); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale SetStatus err = %v, want ErrConflict", err)
	}

	// Terminal statuses do not go through SetStatus.
	if _, err := runs.SetStatus(ctx, "r1", 0, domain.RunSucceeded); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("terminal SetStatus err = %v, want ErrInvalid", err)
	}

	// running -> waiting is the one allowed backwards step.
	waiting, err := runs.SetStatus(ctx, "r1", 0, domain.RunWaiting)
	if err != nil {
		t.Fatalf("SetStatus waiting: %v", err)
	}
	if waiting.Status != domain.RunWaiting {
		t.Errorf("status = %q, want waiting", waiting.Status)
	}

	// A parked run is not work for a worker to claim.
	claimed, err := runs.Claim(ctx, farFuture(), 10, "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("Claim picked up %d runs, want none while r1 waits", len(claimed))
	}
	if active, err := runs.ListActive(ctx, "c1"); err != nil || len(active) != 0 {
		t.Errorf("ListActive = %d runs, err %v; want none while r1 waits", len(active), err)
	}

	// Resuming returns to running, and the step CAS still guards it.
	if _, err := runs.SetStatus(ctx, "r1", 5, domain.RunRunning); !errors.Is(err, store.ErrConflict) {
		t.Errorf("resuming with a stale step err = %v, want ErrConflict", err)
	}
	if _, err := runs.SetStatus(ctx, "r1", 0, domain.RunRunning); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// A running run is claimable once its heartbeat is stale.
	claimed, err = runs.Claim(ctx, farFuture(), 10, "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "r1" {
		t.Fatalf("Claim returned %+v, want the stale running run", claimed)
	}

	finished, err := runs.Finish(ctx, "r1", domain.RunSucceeded, "")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if finished.Status != domain.RunSucceeded || finished.FinishedAt.IsZero() {
		t.Errorf("Finish returned %+v", finished)
	}
	if _, err := runs.SetStatus(ctx, "r1", 1, domain.RunRunning); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a finished run must not resume: err = %v", err)
	}

	if _, err := runs.SetStatus(ctx, "missing", 0, domain.RunRunning); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing run err = %v, want ErrNotFound", err)
	}
}

// farFuture is a stale-before bound no heartbeat can be behind, so a Claim
// only picks up runs that are unconditionally eligible.
func farFuture() time.Time {
	return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
}
