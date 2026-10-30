package anansistore

import (
	"context"
	"strings"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/persistence/base"
	"github.com/asaidimu/go-anansi/v8/core/query"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// conversationRepo mirrors store/memory's conversationRepo, including its
// quirks: Get is not owner-filtered, List is, Create scope violations are
// ErrInvalid, and Rev starts at 1 (anansi's metadata version).
type conversationRepo struct{ r *repos }

func convRefKey(h *domain.TurnRef) string {
	if h == nil {
		return ""
	}
	return turnRefKey(h.ID, h.Version)
}

func refFromKey(key string) *domain.TurnRef {
	if key == "" {
		return nil
	}
	id, version := splitRefKey(key)
	if id == "" {
		return nil
	}
	return &domain.TurnRef{ID: id, Version: version}
}

func decodeConversation(d data.Documenter) (domain.Conversation, error) {
	c := domain.Conversation{
		ID:         sget(d, "conversation_id"),
		Owner:      sget(d, "owner"),
		Title:      sget(d, "title"),
		RoleID:     sget(d, "role_id"),
		Head:       refFromKey(sget(d, "head_key")),
		ForkedFrom: refFromKey(sget(d, "forked_from_key")),
		Status:     domain.ConversationStatus(sget(d, "status")),
		Rev:        revOf(d),
		CreatedAt:  parseTime(sget(d, "created_at")),
		UpdatedAt:  parseTime(sget(d, "updated_at")),
	}
	var err error
	if c.Topics, err = stringsOf(d, "topics_json"); err != nil {
		return domain.Conversation{}, err
	}
	if c.Attached, err = refsOf(d, "attached_json"); err != nil {
		return domain.Conversation{}, err
	}
	if c.PreferenceKeys, err = refsOf(d, "preference_keys_json"); err != nil {
		return domain.Conversation{}, err
	}
	c.Constraints = rawOf(d, "constraints")
	return c, nil
}

func conversationDoc(c domain.Conversation, createdAt, updatedAt string) (data.Documenter, error) {
	m := map[string]any{
		"conversation_id": c.ID,
		"owner":           c.Owner,
		"title":           c.Title,
		"role_id":         c.RoleID,
		"head_key":        convRefKey(c.Head),
		"forked_from_key": convRefKey(c.ForkedFrom),
		"status":          string(c.Status),
		"created_at":      createdAt,
		"updated_at":      updatedAt,
	}
	if c.Topics != nil {
		s, ok := encodeJSON(c.Topics)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["topics_json"] = s
	}
	if c.Attached != nil {
		s, ok := encodeJSON(c.Attached)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["attached_json"] = s
	}
	if c.PreferenceKeys != nil {
		s, ok := encodeJSON(c.PreferenceKeys)
		if !ok {
			return nil, store.ErrInvalid
		}
		m["preference_keys_json"] = s
	}
	if len(c.Constraints) > 0 {
		m["constraints"] = string(c.Constraints)
	}
	return data.NewDocument(m)
}

func (repo *conversationRepo) coll() base.Collection { return repo.r.st.col("conversations") }

func (repo *conversationRepo) Create(ctx context.Context, c domain.Conversation) (domain.Conversation, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Conversation{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Conversation{}, err
	}
	if c.ID == "" || !inScope(repo.r.scope, c.Owner) {
		return domain.Conversation{}, store.ErrInvalid
	}
	now := fmtTime(repo.r.st.now())
	stored := c
	stored.Rev = 1
	if stored.Status == "" {
		stored.Status = domain.StatusActive
	}
	var out domain.Conversation
	err = repo.r.write(actx, func(wctx context.Context) error {
		doc, err := conversationDoc(stored, now, now)
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
		got, err := firstDoc(wctx, repo.coll(), eq("conversation_id", c.ID))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeConversation(got)
		return err
	})
	if err != nil {
		return domain.Conversation{}, err
	}
	return out, nil
}

func (repo *conversationRepo) Get(ctx context.Context, id string) (domain.Conversation, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Conversation{}, err
	}
	d, err := firstDoc(actx, repo.coll(), eq("conversation_id", id))
	if err != nil {
		return domain.Conversation{}, err
	}
	if d == nil {
		return domain.Conversation{}, store.ErrNotFound
	}
	defer d.Release()
	return decodeConversation(d)
}

func (repo *conversationRepo) List(ctx context.Context, f store.ConversationFilter, p store.PageRequest) (store.Page[domain.Conversation], error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return store.Page[domain.Conversation]{}, err
	}
	var filters []query.QueryFilter
	if !repo.r.scope.System {
		filters = append(filters, eq("owner", repo.r.scope.Owner))
	}
	if f.RoleID != "" {
		filters = append(filters, eq("role_id", f.RoleID))
	}
	if f.Status != "" {
		filters = append(filters, eq("status", f.Status))
	}
	if f.UpdatedAfter != nil {
		filters = append(filters, condStr("updated_at", query.ComparisonOperatorGt, fmtTime(*f.UpdatedAfter)))
	}

	var out []domain.Conversation
	err = readDocs(actx, repo.coll(), newQuery(0, filters...), func(d data.Documenter) error {
		c, err := decodeConversation(d)
		if err != nil {
			return err
		}
		if len(f.IDs) > 0 && !containsString(f.IDs, c.ID) {
			return nil
		}
		if !topicsMatch(c.Topics, f.Topics) {
			return nil
		}
		out = append(out, c)
		return nil
	})
	if err != nil {
		return store.Page[domain.Conversation]{}, err
	}
	return paginate(out, func(c domain.Conversation) string { return c.ID }, p.Limit, p.Cursor, p.Order), nil
}

func (repo *conversationRepo) Update(ctx context.Context, id string, expectedRev int64, patch store.ConversationPatch) (domain.Conversation, error) {
	return repo.mutate(ctx, id, expectedRev, patch, nil, false)
}

func (repo *conversationRepo) SetHead(ctx context.Context, id string, expectedRev int64, head *domain.TurnRef) (domain.Conversation, error) {
	return repo.mutate(ctx, id, expectedRev, store.ConversationPatch{}, head, true)
}

// mutate is the shared CAS path for Update and SetHead: pre-read inside the
// mini transaction, check the rev, partial Update with the metadata Version
// as the CAS, then read back (anansi bumps the version for us).
func (repo *conversationRepo) mutate(ctx context.Context, id string, expectedRev int64, patch store.ConversationPatch, head *domain.TurnRef, setHead bool) (domain.Conversation, error) {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return domain.Conversation{}, err
	}
	var out domain.Conversation
	err = repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("conversation_id", id))
		if err != nil {
			return err
		}
		if d == nil {
			return store.ErrNotFound
		}
		cur, err := decodeConversation(d)
		d.Release()
		if err != nil {
			return err
		}
		if cur.Rev != expectedRev {
			return store.ErrConflict
		}

		set := map[string]any{"updated_at": fmtTime(repo.r.st.now())}
		if patch.Title != nil {
			set["title"] = *patch.Title
		}
		if patch.RoleID != nil {
			set["role_id"] = *patch.RoleID
		}
		if patch.Topics != nil {
			s, ok := encodeJSON(*patch.Topics)
			if !ok {
				return store.ErrInvalid
			}
			set["topics_json"] = s
		}
		if patch.Attached != nil {
			s, ok := encodeJSON(*patch.Attached)
			if !ok {
				return store.ErrInvalid
			}
			set["attached_json"] = s
		}
		if patch.PreferenceKeys != nil {
			s, ok := encodeJSON(*patch.PreferenceKeys)
			if !ok {
				return store.ErrInvalid
			}
			set["preference_keys_json"] = s
		}
		if patch.Constraints != nil {
			set["constraints"] = string(*patch.Constraints)
		}
		if patch.Status != nil {
			set["status"] = string(*patch.Status)
		}
		if setHead {
			set["head_key"] = convRefKey(head)
		}

		setDoc, err := data.NewDocument(set)
		if err != nil {
			return store.ErrInvalid
		}
		rev := int(expectedRev)
		res, err := repo.coll().Update(wctx, &base.CollectionUpdate{
			Set:            setDoc,
			Filter:         pf(eq("conversation_id", id)),
			Version:        &rev,
			ReturnDocument: rdPtr(),
		})
		if err != nil {
			return translate(err)
		}
		if res.Count == 0 {
			return store.ErrConflict
		}
		got, err := firstDoc(wctx, repo.coll(), eq("conversation_id", id))
		if err != nil {
			return err
		}
		if got == nil {
			return store.ErrInternal
		}
		defer got.Release()
		out, err = decodeConversation(got)
		return err
	})
	if err != nil {
		return domain.Conversation{}, err
	}
	return out, nil
}

func (repo *conversationRepo) Delete(ctx context.Context, id string, expectedRev int64) error {
	actx, err := repo.r.ready(ctx)
	if err != nil {
		return err
	}
	return repo.r.write(actx, func(wctx context.Context) error {
		d, err := firstDoc(wctx, repo.coll(), eq("conversation_id", id))
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
		n, err := repo.coll().Delete(wctx, pf(eq("conversation_id", id)), false)
		if err != nil {
			return translate(err)
		}
		if n == 0 {
			return store.ErrConflict
		}
		return nil
	})
}

// createStatusError maps a CreateOne result that failed without a Go error
// onto the port sentinels (the probe showed most failures arrive as a Go
// error with the status also set; this covers status-only results).
func createStatusError(res base.CreateResult) error {
	msg := ""
	if res.Error != nil {
		msg = res.Error.Error()
	}
	if strings.Contains(msg, "UNIQUE") || strings.Contains(msg, "unique constraint") {
		return store.ErrAlreadyExists
	}
	switch res.Status {
	case base.StatusFailedValidation, base.StatusFailedPersistence:
		return store.ErrInvalid
	}
	return store.ErrInternal
}
