package anansistore

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// runRepo mirrors store/memory's runRepo: run records are unscoped (any
// scope sees them), step CAS guards Checkpoint/SetStatus, terminal statuses
// are rejected by SetStatus, and Claim leases queued or stale-running runs
// to a worker.
type runRepo struct{ r *repos }

func decodeRun(d data.Documenter) (domain.Run, error) {
	run := domain.Run{
		ID:             sget(d, "run_id"),
		ConversationID: sget(d, "conversation_id"),
		Status:         domain.RunStatus(sget(d, "status")),
		Step:           iget(d, "step"),
		Worker:         sget(d, "worker"),
		HeartbeatAt:    parseTime(sget(d, "heartbeat_at")),
		StartedAt:      parseTime(sget(d, "started_at")),
		FinishedAt:     parseTime(sget(d, "finished_at")),
		Failure:        sget(d, "failure"),
	}
	if d.HasKey("state") {
		// State is opaque: stored and returned as exact bytes, never parsed
		// (mirrors memory, which hands back the caller's json.RawMessage).
		run.State = json.RawMessage(sget(d, "state"))
	}
	return run, nil
}

func runDoc(run domain.Run) (data.Documenter, error) {
	m := map[string]any{
		"run_id":          run.ID,
		"conversation_id": run.ConversationID,
		"status":          string(run.Status),
		"step":            run.Step,
		"worker":          run.Worker,
		"heartbeat_at":    fmtTime(run.HeartbeatAt),
		"started_at":      fmtTime(run.StartedAt),
		"finished_at":     fmtTime(run.FinishedAt),
		"failure":         run.Failure,
	}
	if run.State != nil {
		m["state"] = string(run.State)
	}
	return data.NewDocument(m)
}

func (repo *runRepo) coll() base.Collection { return repo.r.st.col("runs") }

func (repo *runRepo) Create(ctx context.Context, run domain.Run) (domain.Run, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	if run.ID == "" || run.ConversationID == "" {
		return domain.Run{}, store.ErrInvalid
	}
	stored := run
	if stored.Status == "" {
		stored.Status = domain.RunQueued
	}
	now := repo.r.st.now()
	if stored.StartedAt.IsZero() {
		stored.StartedAt = now
	}
	stored.HeartbeatAt = now // memory always stamps this, caller value ignored
	var out domain.Run
	err = repo.r.write(actx, func(wctx context.Context) error {
		doc, err := runDoc(stored)
		if err != nil {
			return err
		}
		res, err := repo.coll().CreateOne(wctx, doc)
		if err != nil {
			return translate(err)
		}
		if res.Status != base.StatusCreated {
			return createStatusError(res)
		}
		got, err := firstDoc(wctx, repo.coll(), eq("run_id", run.ID))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeRun(got)
		return err
	})
	if err != nil {
		return domain.Run{}, err
	}
	return out, nil
}

func (repo *runRepo) Get(ctx context.Context, id string) (domain.Run, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	d, err := firstDoc(actx, repo.coll(), eq("run_id", id))
	if err != nil {
		return domain.Run{}, err
	}
	if d == nil {
		return domain.Run{}, store.ErrNotFound
	}
	defer d.Release()
	return decodeRun(d)
}

// updateRun is the shared write path: pre-read, optional predicate (step CAS
// and friends) returning a sentinel, partial Set, read-back.
func (repo *runRepo) updateRun(ctx context.Context, id string, check func(run domain.Run) error, set map[string]any) (domain.Run, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	var out domain.Run
	err = repo.r.write(actx, func(wctx context.Context) error {
		out = domain.Run{}
		d, err := firstDoc(wctx, repo.coll(), eq("run_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		cur, err := decodeRun(d)
		d.Release()
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(cur); err != nil {
				return err
			}
		}
		setDoc, err := data.NewDocument(set)
		if err != nil {
			return store.ErrInvalid
		}
		res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
			Set:            setDoc,
			Filter:         pf(eq("run_id", id)),
			ReturnDocument: rdPtr(),
		})
		if err != nil {
			return translate(err)
		}
		if res.Count == 0 {
			return store.ErrConflict
		}
		got, err := firstDoc(wctx, repo.coll(), eq("run_id", id))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeRun(got)
		return err
	})
	if err != nil {
		return domain.Run{}, err
	}
	return out, nil
}

// Checkpoint advances the step with a compare-and-swap on expectedStep.
func (repo *runRepo) Checkpoint(ctx context.Context, id string, expectedStep int, state json.RawMessage) (domain.Run, error) {
	set := map[string]any{
		"step":         expectedStep + 1,
		"heartbeat_at": fmtTime(repo.r.st.now()),
	}
	if state != nil {
		set["state"] = string(state)
	}
	return repo.updateRun(ctx, id, func(run domain.Run) error {
		if run.Step != expectedStep {
			return store.ErrConflict
		}
		return nil
	}, set)
}

// SetStatus moves a run between non-terminal states with the same step CAS;
// a terminal target is ErrInvalid and a terminal run is ErrConflict.
func (repo *runRepo) SetStatus(ctx context.Context, id string, expectedStep int, status domain.RunStatus) (domain.Run, error) {
	switch status {
	case domain.RunQueued, domain.RunRunning, domain.RunWaiting:
	default:
		return domain.Run{}, store.ErrInvalid
	}
	return repo.updateRun(ctx, id, func(run domain.Run) error {
		if run.Status.IsTerminal() {
			return store.ErrConflict
		}
		if run.Step != expectedStep {
			return store.ErrConflict
		}
		return nil
	}, map[string]any{
		"status":       string(status),
		"heartbeat_at": fmtTime(repo.r.st.now()),
	})
}

func (repo *runRepo) Heartbeat(ctx context.Context, id string, worker string) error {
	_, err := repo.updateRun(ctx, id, nil, map[string]any{
		"worker":       worker,
		"heartbeat_at": fmtTime(repo.r.st.now()),
	})
	return err
}

// Finish stores a terminal status (no current-state check — memory overwrites
// a finished run's terminal status too).
func (repo *runRepo) Finish(ctx context.Context, id string, status domain.RunStatus, failure string) (domain.Run, error) {
	switch status {
	case domain.RunSucceeded, domain.RunFailed, domain.RunCancelled:
	default:
		return domain.Run{}, store.ErrInvalid
	}
	return repo.updateRun(ctx, id, nil, map[string]any{
		"status":      string(status),
		"failure":     failure,
		"finished_at": fmtTime(repo.r.st.now()),
	})
}

// Claim leases eligible runs to a worker inside one transaction: queued runs
// and running runs whose heartbeat is older than staleBefore (the crash
// recovery path), sorted by ID, capped at limit, each flipped to running
// with the claimant's worker id and a fresh heartbeat.
func (repo *runRepo) Claim(ctx context.Context, staleBefore time.Time, limit int, worker string) ([]domain.Run, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	if worker == "" || limit < 1 {
		return nil, store.ErrInvalid
	}
	now := repo.r.st.now()
	var claimed []domain.Run
	err = repo.r.write(actx, func(wctx context.Context) error {
		claimed = nil
		type cand struct {
			run domain.Run
		}
		var eligible []cand
		collect := func(status domain.RunStatus) error {
			return readDocs(wctx, repo.coll(), newQuery(0, eq("status", string(status))), func(d data.Documenter) error {
				run, err := decodeRun(d)
				if err != nil {
					return err
				}
				if run.Status == domain.RunQueued {
					eligible = append(eligible, cand{run})
					return nil
				}
				// running: only stale heartbeats are reclaimable
				if run.HeartbeatAt.Before(staleBefore) {
					eligible = append(eligible, cand{run})
				}
				return nil
			})
		}
		if err := collect(domain.RunQueued); err != nil {
			return err
		}
		if err := collect(domain.RunRunning); err != nil {
			return err
		}
		sort.Slice(eligible, func(i, j int) bool { return eligible[i].run.ID < eligible[j].run.ID })
		if len(eligible) > limit {
			eligible = eligible[:limit]
		}
		for _, e := range eligible {
			setDoc, err := data.NewDocument(map[string]any{
				"status":       string(domain.RunRunning),
				"worker":       worker,
				"heartbeat_at": fmtTime(now),
			})
			if err != nil {
				return store.ErrInvalid
			}
			res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
				Set:            setDoc,
				Filter:         pf(eq("run_id", e.run.ID)),
				ReturnDocument: rdPtr(),
			})
			if err != nil {
				return translate(err)
			}
			if res.Count == 0 {
				continue // lost the lease; someone else has it
			}
			got, err := firstDoc(wctx, repo.coll(), eq("run_id", e.run.ID))
			if err != nil {
				return err
			}
			if got == nil {
				return store.ErrInternal
			}
			claimedRun, err := decodeRun(got)
			got.Release()
			if err != nil {
				return err
			}
			claimed = append(claimed, claimedRun)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// ListActive returns queued and running runs for a conversation, sorted by
// ID (runs are unscoped — no owner filter, like memory).
func (repo *runRepo) ListActive(ctx context.Context, conversationID string) ([]domain.Run, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.Run
	err = readDocs(actx, repo.coll(), newQuery(0, eq("conversation_id", conversationID)), func(d data.Documenter) error {
		run, err := decodeRun(d)
		if err != nil {
			return err
		}
		if run.Status != domain.RunQueued && run.Status != domain.RunRunning {
			return nil
		}
		out = append(out, run)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
