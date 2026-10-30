package anansistore

import (
	"context"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// roleRepo mirrors store/memory's roleRepo: Put is a full replace with a
// compare-and-swap (create iff expectedRev==0), and Rev comes from the
// anansi metadata version.
type roleRepo struct{ r *repos }

func decodeRole(d data.Documenter) (domain.Role, error) {
	role := domain.Role{
		ID:          sget(d, "role_id"),
		Owner:       sget(d, "owner"),
		Name:        sget(d, "name"),
		Persona:     sget(d, "persona"),
		Constraints: rawOf(d, "constraints"),
		Rev:         revOf(d),
	}
	var err error
	if role.PreferenceKeys, err = refsOf(d, "preference_keys_json"); err != nil {
		return domain.Role{}, err
	}
	if role.Topics, err = stringsOf(d, "topics_json"); err != nil {
		return domain.Role{}, err
	}
	return role, nil
}

func roleDoc(role domain.Role) (data.Documenter, error) {
	m := map[string]any{
		"role_id": role.ID,
		"owner":   role.Owner,
		"name":    role.Name,
		"persona": role.Persona,
	}
	if role.PreferenceKeys != nil {
		s, ok := encodeJSON(role.PreferenceKeys)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["preference_keys_json"] = s
	}
	if role.Topics != nil {
		s, ok := encodeJSON(role.Topics)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["topics_json"] = s
	}
	if role.Constraints != nil {
		m["constraints"] = string(role.Constraints)
	}
	return data.NewDocument(m)
}

// roleReplaceDoc is roleDoc for a full replace: a nil optional field is an
// explicit SQL NULL in the Set, so the replace clears the stored value the
// way memory's whole-record overwrite does (an Update Set only touches the
// keys it carries).
func roleReplaceDoc(role domain.Role) (data.Documenter, error) {
	m := map[string]any{
		"role_id": role.ID,
		"owner":   role.Owner,
		"name":    role.Name,
		"persona": role.Persona,
	}
	prefs, ok := encodeJSON(role.PreferenceKeys)
	if !ok && role.PreferenceKeys != nil {
		return nil, store.ErrInvalid
	}
	if role.PreferenceKeys == nil {
		m["preference_keys_json"] = nil
	} else {
		m["preference_keys_json"] = prefs
	}
	topics, ok := encodeJSON(role.Topics)
	if !ok && role.Topics != nil {
		return nil, store.ErrInvalid
	}
	if role.Topics == nil {
		m["topics_json"] = nil
	} else {
		m["topics_json"] = topics
	}
	if role.Constraints == nil {
		m["constraints"] = nil
	} else {
		m["constraints"] = string(role.Constraints)
	}
	return data.NewDocument(m)
}

func (repo *roleRepo) coll() base.Collection { return repo.r.st.col("roles") }

func (repo *roleRepo) Put(ctx context.Context, role domain.Role, expectedRev int64) (domain.Role, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Role{}, err
	}
	if role.ID == "" || !inScope(repo.r.scope, role.Owner) {
		return domain.Role{}, store.ErrInvalid
	}
	var out domain.Role
	err = repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("role_id", role.ID))
		if err != nil {
			return err
		}
		if d == nil {
			if expectedRev != 0 {
				return store.ErrConflict
			}
			stored := role
			stored.Rev = 1
			doc, err := roleDoc(stored)
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
			got, err := firstDoc(wctx, repo.coll(), eq("role_id", role.ID))
			if err != nil {
				return err
			}
			if got == nil {
				return store.ErrInternal
			}
			defer got.Release()
			out, err = decodeRole(got)
			return err
		}
		cur, err := decodeRole(d)
		d.Release()
		if err != nil {
			return err
		}
		if cur.Rev != expectedRev {
			return store.ErrConflict
		}
		doc, err := roleReplaceDoc(role)
		if err != nil {
			return err
		}
		rev := int(cur.Rev)
		res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
			Set:            doc,
			Filter:         pf(eq("role_id", role.ID)),
			Version:        &rev,
			ReturnDocument: rdPtr(),
		})
		if err != nil {
			return translate(err)
		}
		if res.Count == 0 {
			return store.ErrConflict
		}
		got, err := firstDoc(wctx, repo.coll(), eq("role_id", role.ID))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeRole(got)
		return err
	})
	if err != nil {
		return domain.Role{}, err
	}
	return out, nil
}

func (repo *roleRepo) Get(ctx context.Context, id string) (domain.Role, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Role{}, err
	}
	d, err := firstDoc(actx, repo.coll(), eq("role_id", id))
	if err != nil {
		return domain.Role{}, err
	}
	if d == nil {
		return domain.Role{}, store.ErrNotFound
	}
	defer d.Release()
	return decodeRole(d)
}

func (repo *roleRepo) List(ctx context.Context, f store.RoleFilter, p store.PageRequest) (store.Page[domain.Role], error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return store.Page[domain.Role]{}, err
	}
	var filters []query.QueryFilter
	if !repo.r.scope.System {
		filters = append(filters, eq("owner", repo.r.scope.Owner))
	}
	var out []domain.Role
	err = readDocs(actx, repo.coll(), newQuery(0, filters...), func(d data.Documenter) error {
		role, err := decodeRole(d)
		if err != nil {
			return err
		}
		if len(f.IDs) > 0 && !containsString(f.IDs, role.ID) {
			return nil
		}
		if !topicsMatch(role.Topics, f.Topics) {
			return nil
		}
		out = append(out, role)
		return nil
	})
	if err != nil {
		return store.Page[domain.Role]{}, err
	}
	return paginate(out, func(role domain.Role) string { return role.ID }, p.Limit, p.Cursor, p.Order), nil
}

func (repo *roleRepo) Delete(ctx context.Context, id string, expectedRev int64) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	return repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("role_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		rev := revOf(d)
		d.Release()
		if rev != expectedRev {
			return store.ErrConflict
		}
		n, err := repo.coll().Delete(wctx, pf(eq("role_id", id)), false)
		if err != nil {
			return translate(err)
		}
		if n == 0 {
			return store.ErrConflict
		}
		return nil
	})
}
