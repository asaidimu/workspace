package anansistore

import (
	"context"
	"fmt"
	"time"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// blobRepo mirrors store/memory's blobRepo: metadata only (bytes live in the
// blob store), checksum dedupe is GLOBAL (not per owner — the memory oracle
// matches any existing row), and holder links live in a side collection.
type blobRepo struct{ r *repos }

func decodeBlob(d data.Documenter) (domain.Blob, error) {
	// @note #ctp1dk issue : Manual struct binding?
	//
	// Anansi has utilities to automatically bind documents to
	// structs. Check data.Documenter.d.BindToTag()
	// This problem seems to be replicated in all repositories
	b := domain.Blob{
		ID:        sget(d, "blob_id"),
		Owner:     sget(d, "owner"),
		Checksum:  sget(d, "checksum"),
		Size:      int64(iget(d, "size_bytes")),
		MediaType: sget(d, "media_type"),
		Name:      sget(d, "blob_name"),
		CreatedAt: parseTime(sget(d, "created_at")),
	}
	if d.HasKey("remotes_json") {
		if err := unmarshalStrict([]byte(sget(d, "remotes_json")), &b.Remotes); err != nil {
			return domain.Blob{}, err
		}
	}
	return b, nil
}

func blobDoc(b domain.Blob) (data.Documenter, error) {
	remotes := b.Remotes
	if remotes == nil {
		remotes = map[string]domain.RemoteRef{}
	}
	s, ok := encodeJSON(remotes)
	if !ok {
		return nil, store.ErrInvalid
	}

	// @note #ple99i issue : Use of depracated interfaces.
	//
	// Anansi has deprecated data.Document. Prefer the more
	// robust document.Document
	return data.NewDocument(map[string]any{
		"blob_id":      b.ID,
		"owner":        b.Owner,
		"checksum":     b.Checksum,
		"size_bytes":   int(b.Size),
		"media_type":   b.MediaType,
		"blob_name":    b.Name,
		"remotes_json": s,
		"created_at":   fmtTime(b.CreatedAt),
	})
}

func decodeEntityRef(d data.Documenter) domain.EntityRef {
	return domain.EntityRef{
		Kind:    domain.EntityKind(sget(d, "kind")),
		ID:      sget(d, "entity_id"),
		Version: iget(d, "entity_version"),
	}
}

func refKeyOf(blobID string, holder domain.EntityRef) string {
	return fmt.Sprintf("%s|%s|%s|%d", blobID, holder.Kind, holder.ID, holder.Version)
}

func (repo *blobRepo) coll() base.Collection    { return repo.r.st.col("blobs") }
func (repo *blobRepo) refColl() base.Collection { return repo.r.st.col("blob_refs") }

// Register records blob metadata, deduplicating on checksum globally: any
// existing row with the same checksum is returned with created=false, which
// is how two owners sharing identical bytes share one record (memory's rule).
func (repo *blobRepo) Register(ctx context.Context, b domain.Blob) (domain.Blob, bool, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Blob{}, false, err
	}
	if b.Checksum == "" || !inScope(repo.r.scope, b.Owner) {
		return domain.Blob{}, false, store.ErrInvalid
	}
	var (
		out     domain.Blob
		created bool
	)

	// @note #z18thn issue : Performance Cost
	//
	// What exactly is the performance footprint of this callback.
	// Loading all records into memory to check for a duplicate is
	// potentially wastefull.
	// Consider a unique index on the checksum field instead to push
	// cost down to the persistence layer
	err = repo.r.write(actx, func(wctx context.Context) error {
		out, created = domain.Blob{}, false
		existing := domain.Blob{}
		found := false
		err := readDocs(wctx, repo.coll(), newQuery(0, eq("checksum", b.Checksum)), func(d data.Documenter) error {
			if found {
				return nil
			}
			var e domain.Blob
			var err error
			// see the issue above. Decoding
			if e, err = decodeBlob(d); err != nil {
				return err
			}
			existing, found = e, true
			return nil
		})
		if err != nil {
			return err
		}
		if found {
			out, created = existing, false
			return nil
		}
		if b.ID == "" {
			return store.ErrInvalid
		}
		d, err := firstDoc(wctx, repo.coll(), eq("blob_id", b.ID))
		if err != nil {
			return err
		}
		if d != nil {
			d.Release()
			return store.ErrAlreadyExists
		}
		stored := b
		if stored.CreatedAt.IsZero() {
			stored.CreatedAt = repo.r.st.now()
		}
		doc, err := blobDoc(stored)
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
		got, err := firstDoc(wctx, repo.coll(), eq("blob_id", b.ID))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeBlob(got)
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return domain.Blob{}, false, err
	}
	return out, created, nil
}

func (repo *blobRepo) Get(ctx context.Context, id string) (domain.Blob, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Blob{}, err
	}
	d, err := firstDoc(actx, repo.coll(), eq("blob_id", id))
	if err != nil {
		return domain.Blob{}, err
	}
	if d == nil {
		return domain.Blob{}, store.ErrNotFound
	}
	defer d.Release()
	return decodeBlob(d)
}

// GetByChecksum is not owner-filtered (memory scans every row).
func (repo *blobRepo) GetByChecksum(ctx context.Context, checksum string) (domain.Blob, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Blob{}, err
	}
	d, err := firstDoc(actx, repo.coll(), eq("checksum", checksum))
	if err != nil {
		return domain.Blob{}, err
	}
	if d == nil {
		return domain.Blob{}, store.ErrNotFound
	}
	defer d.Release()
	return decodeBlob(d)
}

func (repo *blobRepo) SetRemote(ctx context.Context, id string, provider string, remote domain.RemoteRef) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	return repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("blob_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		b, err := decodeBlob(d)
		d.Release()
		if err != nil {
			return err
		}
		if b.Remotes == nil {
			b.Remotes = map[string]domain.RemoteRef{}
		}
		b.Remotes[provider] = remote
		s, ok := encodeJSON(b.Remotes)
		if !ok {
			return store.ErrInvalid
		}
		setDoc, err := data.NewDocument(map[string]any{"remotes_json": s})
		if err != nil {
			return store.ErrInvalid
		}
		res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
			Set:            setDoc,
			Filter:         pf(eq("blob_id", id)),
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

func (repo *blobRepo) AddRef(ctx context.Context, id string, holder domain.EntityRef) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	return repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("blob_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		d.Release()
		key := refKeyOf(id, holder)
		existing, err := firstDoc(wctx, repo.refColl(), eq("ref_key", key))
		if err != nil {
			return err
		}
		if existing != nil {
			existing.Release()
			return nil // idempotent, like memory
		}
		doc, err := data.NewDocument(map[string]any{
			"ref_key":        key,
			"blob_id":        id,
			"kind":           string(holder.Kind),
			"entity_id":      holder.ID,
			"entity_version": holder.Version,
		})
		if err != nil {
			return store.ErrInvalid
		}
		res, err := repo.refColl().CreateOne(wctx, doc)
		if err != nil {
			return translate(err)
		}
		if res.Status != base.StatusCreated {
			return createStatusError(res)
		}
		return nil
	})
}

func (repo *blobRepo) RemoveRef(ctx context.Context, id string, holder domain.EntityRef) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	return repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("blob_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		d.Release()
		key := refKeyOf(id, holder)
		existing, err := firstDoc(wctx, repo.refColl(), eq("ref_key", key))
		if err != nil {
			return err
		}
		if existing == nil {
			return nil // removing a link nobody holds is a no-op (memory)
		}
		existing.Release()
		if _, err := repo.refColl().Delete(wctx, pf(eq("ref_key", key)), false); err != nil {
			return translate(err)
		}
		return nil
	})
}

func (repo *blobRepo) Refs(ctx context.Context, id string) ([]domain.EntityRef, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	// Existence check and link read share one snapshot (memory holds its
	// store lock across both).
	var out []domain.EntityRef
	err = repo.r.write(actx, func(wctx context.Context) error {
		out = nil
		d, err := firstDoc(wctx, repo.coll(), eq("blob_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		d.Release()
		return readDocs(wctx, repo.refColl(), newQuery(0, eq("blob_id", id)), func(d data.Documenter) error {
			out = append(out, decodeEntityRef(d))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Orphans lists scope-owned blobs with no holder link and CreatedAt before
// the cutoff, sorted by ID, limited — the same shape as store/memory.
func (repo *blobRepo) Orphans(ctx context.Context, olderThan time.Time, limit int) ([]domain.Blob, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return nil, err
	}
	var filters []query.QueryFilter
	if !repo.r.scope.System {
		filters = append(filters, eq("owner", repo.r.scope.Owner))
	}
	filters = append(filters, condStr("created_at", query.ComparisonOperatorLt, fmtTime(olderThan)))

	var (
		out   []domain.Blob
		orphn []domain.Blob
	)
	err = repo.r.write(actx, func(wctx context.Context) error {
		out, orphn = nil, nil
		// All link rows first: ref counts are per blob id.
		refCounts := map[string]int{}

		//@note #32kfv8 issue : Inefficient data access
		//
		// Anansi provides a powerfull and flexible query language. Here we
		// read all rows, then count them in memory. This problem might be
		// duplicated elsewhere.
		// Anansi also provides a sensible default for the id property, enforced
		// as chronological, uuidv7s. This allows efficient sorting of data
		// See other issues relating to subpar solutions to problems already
		// solved by anansi.
		if err := readDocs(wctx, repo.refColl(), newQuery(0), func(d data.Documenter) error {
			refCounts[sget(d, "blob_id")]++
			return nil
		}); err != nil {
			return err
		}
		if err := readDocs(wctx, repo.coll(), newQuery(0, filters...), func(d data.Documenter) error {
			b, err := decodeBlob(d)
			if err != nil {
				return err
			}
			out = append(out, b)
			return nil
		}); err != nil {
			return err
		}
		for _, b := range out {
			if refCounts[b.ID] > 0 {
				continue
			}
			orphn = append(orphn, b)
		}
		sortBlobsByID(orphn)
		if limit > 0 && len(orphn) > limit {
			orphn = orphn[:limit]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return orphn, nil
}

func sortBlobsByID(bs []domain.Blob) {
	for i := 1; i < len(bs); i++ {
		for j := i; j > 0 && bs[j].ID < bs[j-1].ID; j-- {
			bs[j], bs[j-1] = bs[j-1], bs[j]
		}
	}
}

func (repo *blobRepo) Delete(ctx context.Context, id string) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	return repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("blob_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		d.Release()
		if _, err := repo.coll().Delete(wctx, pf(eq("blob_id", id)), false); err != nil {
			return translate(err)
		}
		// Holder links go with the blob (memory deletes refs[id]).
		var keys []string
		if err := readDocs(wctx, repo.refColl(), newQuery(0, eq("blob_id", id)), func(d data.Documenter) error {
			keys = append(keys, sget(d, "ref_key"))
			return nil
		}); err != nil {
			return err
		}
		for _, k := range keys {
			if _, err := repo.refColl().Delete(wctx, pf(eq("ref_key", k)), false); err != nil {
				return translate(err)
			}
		}
		return nil
	})
}
