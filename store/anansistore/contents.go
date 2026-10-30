package anansistore

import (
	"context"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// contentRepo mirrors store/memory's contentRepo: versions are immutable,
// Put is a CAS on the head version, the head is the max version INCLUDING
// tombstoned rows, and a tombstone marks every version of a key.
type contentRepo struct{ r *repos }

func decodeContent(d data.Documenter) (domain.Content, error) {
	// @note #c0nyaq issue : Replicated pattern
	// @see #ctp1dk
	c := domain.Content{
		Key:        sget(d, "content_id"),
		Version:    iget(d, "version"),
		Owner:      sget(d, "owner"),
		Type:       sget(d, "content_type"),
		Mode:       domain.ContentMode(sget(d, "mode")),
		Value:      rawOf(d, "value"),
		Tombstoned: bget(d, "tombstoned"),
		CreatedAt:  parseTime(sget(d, "created_at")),
	}
	var err error
	if c.Topics, err = stringsOf(d, "topics_json"); err != nil {
		return domain.Content{}, err
	}
	if d.HasKey("meta_json") {
		if err := unmarshalStrict([]byte(sget(d, "meta_json")), &c.Metadata); err != nil {
			return domain.Content{}, err
		}
	}
	return c, nil
}

func contentDoc(c domain.Content) (data.Documenter, error) {
	// @note #c0nyvq issue : Replicated pattern
	// @see #ctp1dk
	m := map[string]any{
		"content_key":  contentPageKey(c.Key, c.Version),
		"content_id":   c.Key,
		"version":      c.Version,
		"owner":        c.Owner,
		"content_type": c.Type,
		"mode":         string(c.Mode),
		"tombstoned":   c.Tombstoned,
		"created_at":   fmtTime(c.CreatedAt),
	}
	if c.Value != nil {
		m["value"] = string(c.Value)
	}
	if c.Topics != nil {
		s, ok := encodeJSON(c.Topics)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["topics_json"] = s
	}
	if c.Metadata != nil {
		s, ok := encodeJSON(c.Metadata)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["meta_json"] = s
	}
	return data.NewDocument(m)
}

func (repo *contentRepo) coll() base.Collection { return repo.r.st.col("contents") }

// headVersion returns the max version of key over ALL rows (tombstoned
// included) inside wctx — store/memory's headVersion scans every ref.
func (repo *contentRepo) headVersion(wctx context.Context, key string) (int, error) {
	head := 0
	err := readDocs(wctx, repo.coll(), newQuery(0, eq("content_id", key)), func(d data.Documenter) error {
		if v := iget(d, "version"); v > head {
			head = v
		}
		return nil
	})
	return head, err
}

// Put writes a new version of a content key with the port's compare-and-swap:
// baseVersion 0 must see an empty key (else ErrAlreadyExists), any other
// base must equal the current head (else ErrConflict).
func (repo *contentRepo) Put(ctx context.Context, c domain.Content, baseVersion int) (domain.Content, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Content{}, err
	}
	if c.Key == "" || !inScope(repo.r.scope, c.Owner) {
		return domain.Content{}, store.ErrInvalid
	}
	if baseVersion < 0 {
		return domain.Content{}, store.ErrInvalid
	}
	stored := c
	var out domain.Content
	err = repo.r.write(actx, func(wctx context.Context) error {
		head, err := repo.headVersion(wctx, c.Key)
		if err != nil {
			return err
		}
		if baseVersion == 0 {
			if head != 0 {
				return store.ErrAlreadyExists
			}
		} else if baseVersion != head {
			return store.ErrConflict
		}
		stored.Version = head + 1
		if stored.CreatedAt.IsZero() {
			stored.CreatedAt = repo.r.st.now()
		}
		stored.Tombstoned = false
		doc, err := contentDoc(stored)
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
		got, err := firstDoc(wctx, repo.coll(), eq("content_key", contentPageKey(stored.Key, stored.Version)))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeContent(got)
		return err
	})
	if err != nil {
		return domain.Content{}, err
	}
	return out, nil
}

// Get reads one version; version 0 means the head, and a tombstoned row (or
// a tombstoned head) reports ErrNotFound exactly like store/memory.
func (repo *contentRepo) Get(ctx context.Context, ref domain.ContentRef) (domain.Content, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Content{}, err
	}
	var out domain.Content
	err = repo.r.write(actx, func(wctx context.Context) error {
		out = domain.Content{}
		v := ref.Version
		if v == 0 {
			v, err = repo.headVersion(wctx, ref.Key)
			if err != nil {
				return err
			}
			if v == 0 {
				return store.ErrNotFound
			}
		}
		d, err := firstDoc(wctx, repo.coll(), eq("content_key", contentPageKey(ref.Key, v)))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		defer d.Release()
		c, err := decodeContent(d)
		if err != nil {
			return err
		}
		if c.Tombstoned {
			return store.ErrNotFound
		}
		out = c
		return nil
	})
	if err != nil {
		return domain.Content{}, err
	}
	return out, nil
}

func (repo *contentRepo) List(ctx context.Context, f store.ContentFilter, p store.PageRequest) (store.Page[domain.Content], error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return store.Page[domain.Content]{}, err
	}
	var filters []query.QueryFilter
	if !repo.r.scope.System {
		filters = append(filters, eq("owner", repo.r.scope.Owner))
	}

	// The head map and the row scan share one read snapshot: run the whole
	// list inside the repo's transaction context.
	var page store.Page[domain.Content]
	err = repo.r.write(actx, func(wctx context.Context) error {
		page = store.Page[domain.Content]{}
		headByKey := map[string]int{}
		err := readDocs(wctx, repo.coll(), newQuery(0, filters...), func(d data.Documenter) error {
			key := sget(d, "content_id")
			v := iget(d, "version")
			if cur, ok := headByKey[key]; !ok || v > cur {
				headByKey[key] = v
			}
			return nil
		})
		if err != nil {
			return err
		}

		var out []domain.Content
		err = readDocs(wctx, repo.coll(), newQuery(0, filters...), func(d data.Documenter) error {
			c, err := decodeContent(d)
			if err != nil {
				return err
			}
			if c.Tombstoned && !f.IncludeTombstoned {
				return nil
			}
			if !f.AllVersions && headByKey[c.Key] != c.Version {
				return nil
			}
			if len(f.Keys) > 0 && !containsString(f.Keys, c.Key) {
				return nil
			}
			if len(f.Types) > 0 && !containsString(f.Types, c.Type) {
				return nil
			}
			if len(f.Modes) > 0 && !containsString(f.Modes, string(c.Mode)) {
				return nil
			}
			if !topicsMatch(c.Topics, f.Topics) {
				return nil
			}
			out = append(out, c)
			return nil
		})
		if err != nil {
			return err
		}
		page = paginate(out, func(c domain.Content) string {
			// The keyset key carries the padded version so AllVersions paging
			// matches store/memory's cursor exactly.
			return c.Key + "/" + pad20(c.Version)
		}, p.Limit, p.Cursor, p.Order)
		return nil
	})
	if err != nil {
		return store.Page[domain.Content]{}, err
	}
	return page, nil
}

func (repo *contentRepo) Versions(ctx context.Context, key string) ([]domain.ContentRef, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.ContentRef
	err = readDocs(actx, repo.coll(), newQuery(0, eq("content_id", key)), func(d data.Documenter) error {
		out = append(out, domain.ContentRef{Key: key, Version: iget(d, "version")})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortContentRefs(out)
	return out, nil
}

func sortContentRefs(refs []domain.ContentRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j].Version < refs[j-1].Version; j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}

// Tombstone hides every version of a key (history survives) after a CAS on
// the head version.
func (repo *contentRepo) Tombstone(ctx context.Context, key string, baseVersion int) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	return repo.r.write(actx, func(wctx context.Context) error {
		head, err := repo.headVersion(wctx, key)
		if err != nil {
			return err
		}
		if head == 0 {
			return store.ErrNotFound
		}
		if baseVersion < 0 || baseVersion != head {
			return store.ErrConflict
		}
		var keys []string
		err = readDocs(wctx, repo.coll(), newQuery(0, eq("content_id", key)), func(d data.Documenter) error {
			keys = append(keys, sget(d, "content_key"))
			return nil
		})
		if err != nil {
			return err
		}
		for _, ck := range keys {
			setDoc, err := data.NewDocument(map[string]any{"tombstoned": true})
			if err != nil {
				return store.ErrInvalid
			}
			res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
				Set:            setDoc,
				Filter:         pf(eq("content_key", ck)),
				ReturnDocument: rdPtr(),
			})
			if err != nil {
				return translate(err)
			}
			if res.Count == 0 {
				return store.ErrConflict
			}
		}
		return nil
	})
}
