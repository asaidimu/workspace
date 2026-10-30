package blob

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ChecksumLookup answers the one question a reconcile pass needs of the
// metadata store: does ANY owner's metadata reference this checksum? The
// implementation runs in system scope, because bytes are shared across
// owners through content addressing.
type ChecksumLookup interface {
	References(ctx context.Context, checksum string) (bool, error)
}

// LookupFunc adapts a function to ChecksumLookup.
type LookupFunc func(ctx context.Context, checksum string) (bool, error)

// References implements ChecksumLookup.
func (f LookupFunc) References(ctx context.Context, checksum string) (bool, error) {
	return f(ctx, checksum)
}

// ReconcileOptions bounds one pass.
type ReconcileOptions struct {
	// Grace is how long a checksum must have been observed as an orphan
	// before its bytes may be removed. Zero removes on first sighting.
	Grace time.Duration
	// Limit is the List page size; zero selects a default.
	Limit int
}

// ReconcileReport is what one pass did.
type ReconcileReport struct {
	Examined int // bytes listed this pass
	Orphans  int // listed bytes whose checksum had no metadata
	Deferred int // orphans still inside the grace window
	Removed  int // orphan bytes deleted
}

// defaultReconcilePage is the List page size when no Limit is given.
const defaultReconcilePage = 256

// Reconciler is the job: bytes are written before metadata, so a crash
// between the two leaves bytes with no metadata under any owner, and this
// pass removes them. Grace is tracked by first sighting: a checksum first
// seen this pass is left alone (an upload may be mid-flight), and only a
// checksum that was already an orphan on an earlier pass — at least Grace
// ago — is deleted. Checksums that gain metadata in between are forgotten.
// The sighting state lives in memory, so a process restart restarts the
// grace window; that is safe by construction (worst case the orphan waits
// one more cycle).
type Reconciler struct {
	blobs Store
	meta  ChecksumLookup
	now   func() time.Time

	mu      sync.Mutex
	pending map[string]time.Time
}

// NewReconciler wires a pass over a byte store and a system-scope metadata
// lookup. clock nil means time.Now.
func NewReconciler(blobs Store, meta ChecksumLookup, clock func() time.Time) *Reconciler {
	if clock == nil {
		clock = time.Now
	}
	return &Reconciler{
		blobs:   blobs,
		meta:    meta,
		now:     clock,
		pending: map[string]time.Time{},
	}
}

// Reconcile walks the byte store in List pages and removes bytes whose
// checksum has no metadata under any owner, subject to Grace. It stops at
// the first error and reports what it did up to that point.
func (r *Reconciler) Reconcile(ctx context.Context, opts ReconcileOptions) (ReconcileReport, error) {
	if r == nil || r.blobs == nil || r.meta == nil {
		return ReconcileReport{}, errors.New("blob: reconcile is not wired")
	}
	if err := ctx.Err(); err != nil {
		return ReconcileReport{}, err
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultReconcilePage
	}
	var (
		report ReconcileReport
		after  string
	)
	for {
		infos, err := r.blobs.List(ctx, after, opts.Limit)
		if err != nil {
			return report, err
		}
		for _, info := range infos {
			report.Examined++
			referenced, err := r.meta.References(ctx, info.Checksum)
			if err != nil {
				return report, err
			}
			if referenced {
				// Metadata arrived (late but in time): the next orphan
				// sighting starts fresh if it is ever removed and re-uploaded.
				r.forget(info.Checksum)
				continue
			}
			report.Orphans++
			r.mu.Lock()
			seenAt, sighted := r.pending[info.Checksum]
			switch {
			case !sighted && opts.Grace > 0:
				// First sighting inside a grace window: record it and wait.
				r.pending[info.Checksum] = r.now()
				report.Deferred++
				r.mu.Unlock()
			case sighted && r.now().Sub(seenAt) < opts.Grace:
				report.Deferred++
				r.mu.Unlock()
			default:
				r.mu.Unlock()
				if err := r.blobs.Delete(ctx, info.Checksum); err != nil {
					return report, err
				}
				r.forget(info.Checksum)
				report.Removed++
			}
		}
		if len(infos) < opts.Limit {
			return report, nil
		}
		after = infos[len(infos)-1].Checksum
		if after == "" {
			return report, nil
		}
	}
}

// forget drops a checksum from the sighting set.
func (r *Reconciler) forget(checksum string) {
	r.mu.Lock()
	delete(r.pending, checksum)
	r.mu.Unlock()
}
