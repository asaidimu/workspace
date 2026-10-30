package blob_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/asaidimu/workspace/blob"
	"github.com/asaidimu/workspace/workspacetest"
)

// fixedInstant anchors the reconcile clock the way the harness does.
var fixedInstant = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeClock is a hand-cranked time source for the reconciler.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// put stores bytes and returns their checksum.
func put(t *testing.T, ctx context.Context, bs blob.Store, payload string) string {
	t.Helper()
	info, err := bs.Put(ctx, strings.NewReader(payload), blob.PutOptions{MediaType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return info.Checksum
}

// TestReconcileRemovesOrphansAfterGrace is the crash window: bytes written,
// metadata never registered. The first pass only sights them; a later pass
// past the grace deletes them. Bytes that gained metadata are kept.
func TestReconcileRemovesOrphansAfterGrace(t *testing.T) {
	ctx := context.Background()
	bs := workspacetest.NewMemoryBlobs()
	clock := &fakeClock{t: fixedInstant}

	orphan := put(t, ctx, bs, "orphan-bytes")
	kept := put(t, ctx, bs, "referenced-bytes")

	lookup := blob.LookupFunc(func(_ context.Context, checksum string) (bool, error) {
		return checksum == kept, nil
	})
	r := blob.NewReconciler(bs, lookup, clock.Now)

	// Pass 1: both bytes are listed, the orphan is fresh — deferred.
	rep, err := r.Reconcile(ctx, blob.ReconcileOptions{Grace: time.Hour})
	if err != nil {
		t.Fatalf("reconcile pass 1: %v", err)
	}
	if rep.Examined != 2 || rep.Orphans != 1 || rep.Deferred != 1 || rep.Removed != 0 {
		t.Fatalf("pass 1 report = %+v, want examined=2 orphans=1 deferred=1 removed=0", rep)
	}

	// Inside grace: still deferred.
	clock.Advance(30 * time.Minute)
	rep, err = r.Reconcile(ctx, blob.ReconcileOptions{Grace: time.Hour})
	if err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	if rep.Removed != 0 || rep.Deferred != 1 {
		t.Fatalf("pass 2 report = %+v, want removed=0 deferred=1", rep)
	}

	// Past grace: the orphan goes, the referenced bytes stay.
	clock.Advance(45 * time.Minute)
	rep, err = r.Reconcile(ctx, blob.ReconcileOptions{Grace: time.Hour})
	if err != nil {
		t.Fatalf("reconcile pass 3: %v", err)
	}
	if rep.Removed != 1 || rep.Orphans != 1 {
		t.Fatalf("pass 3 report = %+v, want removed=1 orphans=1", rep)
	}
	if _, err := bs.Stat(ctx, orphan); err == nil {
		t.Errorf("orphan bytes still present after grace")
	}
	if _, err := bs.Stat(ctx, kept); err != nil {
		t.Errorf("referenced bytes deleted: %v", err)
	}

	// A fourth pass is stable: nothing left to remove.
	rep, err = r.Reconcile(ctx, blob.ReconcileOptions{Grace: time.Hour})
	if err != nil {
		t.Fatalf("reconcile pass 4: %v", err)
	}
	if rep.Removed != 0 || rep.Orphans != 0 || rep.Examined != 1 {
		t.Fatalf("pass 4 report = %+v, want a quiet pass over the surviving byte", rep)
	}
}

// TestReconcileZeroGraceRemovesImmediately covers hosts that opt out of the
// grace window: one pass suffices.
func TestReconcileZeroGraceRemovesImmediately(t *testing.T) {
	ctx := context.Background()
	bs := workspacetest.NewMemoryBlobs()
	orphan := put(t, ctx, bs, "no-wait")
	r := blob.NewReconciler(bs, blob.LookupFunc(func(context.Context, string) (bool, error) {
		return false, nil
	}), nil)

	rep, err := r.Reconcile(ctx, blob.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Removed != 1 {
		t.Fatalf("report = %+v, want removed=1", rep)
	}
	if _, err := bs.Stat(ctx, orphan); err == nil {
		t.Errorf("orphan survived a zero-grace pass")
	}
}

// TestReconcilePaginates walks List in pages, so a byte store larger than
// one page is still fully examined.
func TestReconcilePaginates(t *testing.T) {
	ctx := context.Background()
	bs := workspacetest.NewMemoryBlobs()
	for i := 0; i < 5; i++ {
		put(t, ctx, bs, string(rune('a'+i))+"-payload")
	}
	r := blob.NewReconciler(bs, blob.LookupFunc(func(context.Context, string) (bool, error) {
		return false, nil
	}), nil)
	rep, err := r.Reconcile(ctx, blob.ReconcileOptions{Limit: 2})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Examined != 5 || rep.Removed != 5 {
		t.Fatalf("report = %+v, want examined=5 removed=5", rep)
	}
}

// TestReconcileMetadataArrivingInTimeProtectsBytes: a checksum sighted as an
// orphan that gains metadata before grace elapses is never deleted, and its
// sighting is forgotten — if the metadata later disappears, grace starts
// over instead of deleting on the next pass.
func TestReconcileMetadataArrivingInTimeProtectsBytes(t *testing.T) {
	ctx := context.Background()
	bs := workspacetest.NewMemoryBlobs()
	sum := put(t, ctx, bs, "slow-registration")
	referenced := false
	clock := &fakeClock{t: fixedInstant}
	r := blob.NewReconciler(bs, blob.LookupFunc(func(context.Context, string) (bool, error) {
		return referenced, nil
	}), clock.Now)

	// Pass 1: orphan, fresh — deferred.
	if _, err := r.Reconcile(ctx, blob.ReconcileOptions{Grace: time.Minute}); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	// The register command lands: pass 2 sees metadata (still inside grace
	// anyway) and must drop the sighting.
	referenced = true
	clock.Advance(10 * time.Minute)
	rep, err := r.Reconcile(ctx, blob.ReconcileOptions{Grace: time.Minute})
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if rep.Orphans != 0 || rep.Removed != 0 {
		t.Fatalf("pass 2 report = %+v, want metadata to clear the orphan", rep)
	}
	// Metadata disappears (rolled back): grace starts over from this sighting,
	// so an immediate next pass defers instead of deleting.
	referenced = false
	rep, err = r.Reconcile(ctx, blob.ReconcileOptions{Grace: time.Minute})
	if err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if rep.Orphans != 1 || rep.Removed != 0 || rep.Deferred != 1 {
		t.Fatalf("pass 3 report = %+v, want a fresh sighting deferred", rep)
	}
	if _, err := bs.Stat(ctx, sum); err != nil {
		t.Errorf("bytes deleted while metadata existed or inside grace: %v", err)
	}
}

// TestReconcileLookupErrorPropagates: a metadata-store failure stops the pass
// with the partial report rather than deleting anything on a guess.
func TestReconcileLookupErrorPropagates(t *testing.T) {
	ctx := context.Background()
	bs := workspacetest.NewMemoryBlobs()
	put(t, ctx, bs, "a")
	want := errors.New("metadata store down")
	r := blob.NewReconciler(bs, blob.LookupFunc(func(context.Context, string) (bool, error) {
		return false, want
	}), nil)
	rep, err := r.Reconcile(ctx, blob.ReconcileOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if rep.Examined != 1 || rep.Removed != 0 {
		t.Errorf("report = %+v, want the partial pass examined=1 removed=0", rep)
	}
}

// TestReconcileUnwired rejects a reconciler that was never built.
func TestReconcileUnwired(t *testing.T) {
	var r *blob.Reconciler
	if _, err := r.Reconcile(context.Background(), blob.ReconcileOptions{}); err == nil {
		t.Fatal("nil reconciler accepted")
	}
	r2 := blob.NewReconciler(nil, blob.LookupFunc(func(context.Context, string) (bool, error) {
		return false, nil
	}), nil)
	if _, err := r2.Reconcile(context.Background(), blob.ReconcileOptions{}); err == nil {
		t.Fatal("missing byte store accepted")
	}
}

// TestReconcileListErrorPropagates wraps a failing byte store to prove the
// pass surfaces List failures (and Delete failures) instead of reporting
// success.
func TestReconcileListErrorPropagates(t *testing.T) {
	want := errors.New("list failed")
	r := blob.NewReconciler(failingStore{listErr: want},
		blob.LookupFunc(func(context.Context, string) (bool, error) { return false, nil }), nil)
	if _, err := r.Reconcile(context.Background(), blob.ReconcileOptions{}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	r2 := blob.NewReconciler(failingStore{deleteErr: errors.New("delete failed")},
		blob.LookupFunc(func(context.Context, string) (bool, error) { return false, nil }), nil)
	if _, err := r2.Reconcile(context.Background(), blob.ReconcileOptions{}); err == nil {
		t.Fatal("delete failure swallowed")
	}
}

// failingStore fails one of the two operations the pass performs.
type failingStore struct {
	listErr   error
	deleteErr error
}

func (s failingStore) Put(context.Context, io.Reader, blob.PutOptions) (blob.Info, error) {
	return blob.Info{}, errors.New("unexpected Put")
}
func (s failingStore) Open(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, errors.New("unexpected Open")
}
func (s failingStore) Stat(context.Context, string) (blob.Info, error) {
	return blob.Info{}, errors.New("unexpected Stat")
}
func (s failingStore) Delete(context.Context, string) error { return s.deleteErr }
func (s failingStore) List(context.Context, string, int) ([]blob.Info, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return []blob.Info{{Checksum: "sha256:dead", Size: 1}}, nil
}
