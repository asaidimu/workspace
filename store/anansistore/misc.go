package anansistore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// receiptRepo mirrors store/memory's receiptRepo: Reserve dedupes an
// idempotency key (an unexpired receipt returns created=false; an expired
// one is deleted and recreated), and Complete is only valid while the
// receipt is alive. Receipts are unscoped.
type receiptRepo struct{ r *repos }

const defaultReceiptTTL = 24 * time.Hour

func decodeReceipt(d data.Documenter) (domain.Receipt, error) {
	rec := domain.Receipt{
		Key:       sget(d, "receipt_key"),
		State:     domain.ReceiptState(sget(d, "state")),
		ExpiresAt: parseTime(sget(d, "expires_at")),
	}
	if d.HasKey("result") {
		rec.Result = json.RawMessage(sget(d, "result"))
	}
	return rec, nil
}

func (repo *receiptRepo) coll() base.Collection { return repo.r.st.col("receipts") }

func (repo *receiptRepo) Reserve(ctx context.Context, key string, ttl time.Duration) (domain.Receipt, bool, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Receipt{}, false, err
	}
	if key == "" {
		return domain.Receipt{}, false, store.ErrInvalid
	}
	now := repo.r.st.now()
	var (
		out     domain.Receipt
		created bool
	)
	err = repo.r.write(actx, func(wctx context.Context) error {
		out, created = domain.Receipt{}, false
		d, err := firstDoc(wctx, repo.coll(), eq("receipt_key", key))
		if err != nil {
			return err
		}
		if d != nil {
			existing, err := decodeReceipt(d)
			d.Release()
			if err != nil {
				return err
			}
			if now.After(existing.ExpiresAt) {
				if _, err := repo.coll().Delete(wctx, pf(eq("receipt_key", key)), false); err != nil {
					return translate(err)
				}
			} else {
				out, created = existing, false
				return nil
			}
		}
		rec := domain.Receipt{
			Key:       key,
			State:     domain.ReceiptReserved,
			ExpiresAt: now.Add(ttl),
		}
		if ttl == 0 {
			rec.ExpiresAt = now.Add(defaultReceiptTTL)
		}
		set := map[string]any{
			"receipt_key": key,
			"state":       string(rec.State),
			"expires_at":  fmtTime(rec.ExpiresAt),
		}
		if rec.Result != nil {
			set["result"] = string(rec.Result)
		}
		doc, err := data.NewDocument(set)
		if err != nil {
			return store.ErrInvalid
		}
		res, err := repo.coll().CreateOne(wctx, doc)
		if err != nil {
			return translate(err)
		}
		if res.Status != base.StatusCreated {
			return createStatusError(res)
		}
		out, created = rec, true
		return nil
	})
	if err != nil {
		return domain.Receipt{}, false, err
	}
	return out, created, nil
}

func (repo *receiptRepo) Complete(ctx context.Context, key string, result json.RawMessage) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	now := repo.r.st.now()
	return repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("receipt_key", key))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		rec, err := decodeReceipt(d)
		d.Release()
		if err != nil {
			return err
		}
		if now.After(rec.ExpiresAt) {
			if _, err := repo.coll().Delete(wctx, pf(eq("receipt_key", key)), false); err != nil {
				return translate(err)
			}
			return store.ErrNotFound
		}
		set := map[string]any{"state": string(domain.ReceiptCompleted)}
		if result != nil {
			set["result"] = string(result)
		}
		setDoc, err := data.NewDocument(set)
		if err != nil {
			return store.ErrInvalid
		}
		res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
			Set:            setDoc,
			Filter:         pf(eq("receipt_key", key)),
			ReturnDocument: rdPtr(),
		})
		if err != nil {
			return translate(err)
		}
		if res.Count == 0 {
			return store.ErrNotFound
		}
		return nil
	})
}

// settingsRepo mirrors store/memory's settingsRepo: the row is keyed by
// scope ("system" or the owner), Get is keyed (never a list), and Put
// creates iff expectedRev==0 — an empty Owner is defaulted to the scope
// owner only on create, exactly like memory.
type settingsRepo struct{ r *repos }

func settingsKey(scope store.Scope) string {
	if scope.System {
		return "system"
	}
	return scope.Owner
}

func decodeSettings(d data.Documenter) (domain.Settings, error) {
	s := domain.Settings{
		Owner:         sget(d, "owner"),
		Prompt:        sget(d, "prompt"),
		Language:      sget(d, "language"),
		DefaultRoleID: sget(d, "default_role_id"),
		Rev:           revOf(d),
	}
	if err := refsOfInto(d, "global_prefs_json", &s.GlobalPreferenceKeys); err != nil {
		return domain.Settings{}, err
	}
	return s, nil
}

// refsOfInto decodes a []ContentRef field with nil/empty fidelity.
func refsOfInto(d data.Documenter, key string, out *[]domain.ContentRef) error {
	refs, err := refsOf(d, key)
	if err != nil {
		return err
	}
	*out = refs
	return nil
}

// settingsRowDoc builds the document for the scope-keyed settings row.
func settingsRowDoc(key string, s domain.Settings) (data.Documenter, error) {
	m := map[string]any{
		"settings_key":    key,
		"owner":           s.Owner,
		"prompt":          s.Prompt,
		"language":        s.Language,
		"default_role_id": s.DefaultRoleID,
	}
	if s.GlobalPreferenceKeys != nil {
		v, ok := encodeJSON(s.GlobalPreferenceKeys)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["global_prefs_json"] = v
	}
	return data.NewDocument(m)
}

func (repo *settingsRepo) coll() base.Collection { return repo.r.st.col("settings") }

func (repo *settingsRepo) Get(ctx context.Context) (domain.Settings, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Settings{}, err
	}
	d, err := firstDoc(actx, repo.coll(), eq("settings_key", settingsKey(repo.r.scope)))
	if err != nil {
		return domain.Settings{}, err
	}
	if d == nil {
		return domain.Settings{}, store.ErrNotFound
	}
	defer d.Release()
	return decodeSettings(d)
}

func (repo *settingsRepo) Put(ctx context.Context, s domain.Settings, expectedRev int64) (domain.Settings, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Settings{}, err
	}
	key := settingsKey(repo.r.scope)
	var out domain.Settings
	err = repo.r.write(actx, func(wctx context.Context) error {
		out = domain.Settings{}
		d, err := firstDoc(wctx, repo.coll(), eq("settings_key", key))
		if err != nil {
			return err
		}
		if d == nil {
			if expectedRev != 0 {
				return store.ErrConflict
			}
			stored := s
			stored.Rev = 1
			if stored.Owner == "" {
				stored.Owner = repo.r.scope.Owner
			}
			doc, err := settingsRowDoc(key, stored)
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
			got, err := firstDoc(wctx, repo.coll(), eq("settings_key", key))
			if err != nil {
				return err
			}
			if got == nil {
				return store.ErrInternal
			}
			defer got.Release()
			out, err = decodeSettings(got)
			return err
		}
		cur, err := decodeSettings(d)
		d.Release()
		if err != nil {
			return err
		}
		if cur.Rev != expectedRev {
			return store.ErrConflict
		}
		doc, err := settingsRowDoc(key, s)
		if err != nil {
			return err
		}
		rev := int(cur.Rev)
		res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
			Set:            doc,
			Filter:         pf(eq("settings_key", key)),
			Version:        &rev,
			ReturnDocument: rdPtr(),
		})
		if err != nil {
			return translate(err)
		}
		if res.Count == 0 {
			return store.ErrConflict
		}
		got, err := firstDoc(wctx, repo.coll(), eq("settings_key", key))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeSettings(got)
		return err
	})
	if err != nil {
		return domain.Settings{}, err
	}
	return out, nil
}

// --- maintenance and migrator ---

// maintenance mirrors store/memory: the sweeps are no-ops (real collection
// lives in the blob reconcile job and the fault-injection phase).
type maintenance struct{ scope store.Scope }

func (m *maintenance) SweepTurns(ctx context.Context, opts store.SweepOptions) (store.SweepReport, error) {
	return store.SweepReport{}, nil
}

func (m *maintenance) SweepBlobs(ctx context.Context, opts store.SweepOptions) (store.SweepReport, error) {
	return store.SweepReport{}, nil
}

func (m *maintenance) SweepReceipts(ctx context.Context, now time.Time) (store.SweepReport, error) {
	return store.SweepReport{}, nil
}

// migrator re-ensures the collection set (idempotent) and reports version 1.
type migrator struct{ st *Store }

func (m *migrator) Migrate(ctx context.Context) (store.MigrationReport, error) {
	for _, spec := range collectionSpecs {
		if err := m.st.ensureCollection(ctx, spec); err != nil {
			return store.MigrationReport{From: 1, To: 1, Refused: true, Refusal: err.Error()}, nil
		}
	}
	return store.MigrationReport{From: 1, To: 1}, nil
}

func (m *migrator) SchemaVersion(ctx context.Context) (int, error) { return 1, nil }
