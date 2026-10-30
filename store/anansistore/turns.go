package anansistore

import (
	"context"
	"strconv"
	"strings"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// turnRepo mirrors store/memory's turnRepo: turns are immutable, identity is
// (ID, Version), and Versions/Children sort by (id, version).
type turnRepo struct{ r *repos }

// splitRefKey is the inverse of turnCompositeKey ("id@version"); a version
// that does not parse reports id "".
func splitRefKey(key string) (string, int) {
	at := strings.LastIndex(key, "@")
	if at <= 0 || at == len(key)-1 {
		return "", 0
	}
	v, err := strconv.Atoi(key[at+1:])
	if err != nil {
		return "", 0
	}
	return key[:at], v
}

func decodeTurn(d data.Documenter) (domain.Turn, error) {
	t := domain.Turn{
		ID:           sget(d, "turn_id"),
		Version:      iget(d, "turn_version"),
		Owner:        sget(d, "owner"),
		Actor:        domain.Actor{Kind: domain.ActorKind(sget(d, "actor_kind")), ID: sget(d, "actor_id")},
		FinishReason: sget(d, "finish_reason"),
		CreatedAt:    parseTime(sget(d, "created_at")),
	}
	if pk := sget(d, "parent_key"); pk != "" {
		if id, v := splitRefKey(pk); id != "" {
			t.Parent = &domain.TurnRef{ID: id, Version: v}
		}
	}
	if d.HasKey("blocks_json") {
		if err := unmarshalStrict([]byte(sget(d, "blocks_json")), &t.Blocks); err != nil {
			return domain.Turn{}, err
		}
	}
	if d.HasKey("usage_json") {
		if err := unmarshalStrict([]byte(sget(d, "usage_json")), &t.Usage); err != nil {
			return domain.Turn{}, err
		}
	}
	return t, nil
}

func turnDoc(t domain.Turn) (data.Documenter, error) {
	m := map[string]any{
		"turn_key":      turnCompositeKey(t.ID, t.Version),
		"turn_id":       t.ID,
		"turn_version":  t.Version,
		"owner":         t.Owner,
		"actor_kind":    string(t.Actor.Kind),
		"actor_id":      t.Actor.ID,
		"finish_reason": t.FinishReason,
		"created_at":    fmtTime(t.CreatedAt),
	}
	if t.Parent != nil {
		m["parent_key"] = turnRefKey(t.Parent.ID, t.Parent.Version)
	}
	if t.Blocks != nil {
		s, err := encodeBlocks(t.Blocks)
		if err != nil {
			return nil, err
		}
		m["blocks_json"] = s
	}
	if s, ok := encodeJSON(t.Usage); ok {
		m["usage_json"] = s
	}
	return data.NewDocument(m)
}

// encodeBlocks marshals blocks preserving exact number digits (json.Number
// survives as-is; the decoder uses UseNumber on the way back).
func encodeBlocks(blocks []domain.Block) (string, error) {
	s, ok := encodeJSON(blocks)
	if !ok {
		return "", store.ErrInvalid
	}
	return s, nil
}

func (repo *turnRepo) coll() base.Collection { return repo.r.st.col("turns") }

func (repo *turnRepo) Append(ctx context.Context, t domain.Turn) (domain.Turn, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Turn{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Turn{}, err
	}
	if t.ID == "" || t.Version < 1 || !inScope(repo.r.scope, t.Owner) {
		return domain.Turn{}, store.ErrInvalid
	}
	stored := t
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = repo.r.st.now()
	}
	var out domain.Turn
	err = repo.r.write(actx, func(wctx context.Context) error {
		doc, err := turnDoc(stored)
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
		got, err := firstDoc(wctx, repo.coll(), eq("turn_key", turnCompositeKey(t.ID, t.Version)))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeTurn(got)
		return err
	})
	if err != nil {
		return domain.Turn{}, err
	}
	return out, nil
}

func (repo *turnRepo) Get(ctx context.Context, ref domain.TurnRef) (domain.Turn, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Turn{}, err
	}
	d, err := firstDoc(actx, repo.coll(), eq("turn_key", turnCompositeKey(ref.ID, ref.Version)))
	if err != nil {
		return domain.Turn{}, err
	}
	if d == nil {
		return domain.Turn{}, store.ErrNotFound
	}
	defer d.Release()
	return decodeTurn(d)
}

func (repo *turnRepo) Versions(ctx context.Context, id string) ([]domain.TurnRef, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.TurnRef
	err = readDocs(actx, repo.coll(), newQuery(0, eq("turn_id", id)), func(d data.Documenter) error {
		out = append(out, domain.TurnRef{ID: id, Version: iget(d, "turn_version")})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortTurnRefs(out)
	return out, nil
}

func (repo *turnRepo) Children(ctx context.Context, parent domain.TurnRef) ([]domain.TurnRef, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	pk := turnRefKey(parent.ID, parent.Version)
	var out []domain.TurnRef
	err = readDocs(actx, repo.coll(), newQuery(0, eq("parent_key", pk)), func(d data.Documenter) error {
		out = append(out, domain.TurnRef{ID: sget(d, "turn_id"), Version: iget(d, "turn_version")})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortTurnRefs(out)
	return out, nil
}

// Chain walks parent links from head (newest to oldest) with point reads,
// mirroring store/memory's loop exactly: seen-set guard, Until is inclusive
// of the until turn, Limit keeps the newest N. The whole walk runs inside
// the repo's transaction context (a scoped repo opens a mini read
// transaction) so a concurrent append cannot interleave, matching the
// memory store's single-lock walk.
func (repo *turnRepo) Chain(ctx context.Context, head domain.TurnRef, opts store.ChainOptions) ([]domain.Turn, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	var chain []domain.Turn
	err = repo.r.write(actx, func(wctx context.Context) error {
		chain = chain[:0]
		seen := map[domain.TurnRef]bool{}
		cur := head
		for {
			if seen[cur] {
				break
			}
			seen[cur] = true
			d, err := firstDoc(wctx, repo.coll(), eq("turn_key", turnCompositeKey(cur.ID, cur.Version)))
			if err != nil {
				return err
			}
			if d == nil {
				if len(chain) == 0 {
					return store.ErrNotFound
				}
				break
			}
			t, err := decodeTurn(d)
			d.Release()
			if err != nil {
				return err
			}
			chain = append(chain, t)
			if opts.Until != nil && *opts.Until == cur {
				break
			}
			if t.Parent == nil {
				break
			}
			cur = *t.Parent
		}
		if opts.Limit > 0 && len(chain) > opts.Limit {
			chain = chain[:opts.Limit]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return chain, nil
}
