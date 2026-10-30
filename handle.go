package workspace

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/blob"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/store"
)

// scoped is the common half of every sub-handle: the workspace it dispatches
// through and the principal it acts as.
type scoped struct {
	w *Workspace
	p kernel.Principal
}

// dispatch runs one command as the handle's principal. An owner-less
// principal is refused here so the failure reads as FORBIDDEN instead of
// surfacing later as a store argument error.
func dispatch[C kernel.Command, R any](ctx context.Context, s *scoped, c C) (R, error) {
	var zero R
	if s.p.Owner == "" {
		return zero, kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	return kernel.Dispatch[C, R](ctx, s.w.k, s.p, c)
}

// conversations implements the Conversations interface over dispatched
// commands.
type conversations struct{ scoped }

func (c *conversations) Create(ctx context.Context, in CreateConversation) (domain.Conversation, error) {
	return dispatch[createConversationCmd, domain.Conversation](ctx, &c.scoped,
		createConversationCmd{ownerField: ownerField{Owner: c.p.Owner}, In: in})
}

func (c *conversations) Get(ctx context.Context, id string) (domain.Conversation, error) {
	return dispatch[getConversationCmd, domain.Conversation](ctx, &c.scoped,
		getConversationCmd{ownerField: ownerField{Owner: c.p.Owner}, ID: id})
}

func (c *conversations) List(ctx context.Context, f store.ConversationFilter, p store.PageRequest) (store.Page[domain.Conversation], error) {
	return dispatch[listConversationsCmd, store.Page[domain.Conversation]](ctx, &c.scoped,
		listConversationsCmd{ownerField: ownerField{Owner: c.p.Owner}, Filter: f, Page: p})
}

func (c *conversations) Update(ctx context.Context, id string, in UpdateConversation) (domain.Conversation, error) {
	return dispatch[updateConversationCmd, domain.Conversation](ctx, &c.scoped,
		updateConversationCmd{ownerField: ownerField{Owner: c.p.Owner}, ID: id, In: in})
}

func (c *conversations) Fork(ctx context.Context, id string, at domain.TurnRef, title string) (domain.Conversation, error) {
	return dispatch[forkConversationCmd, domain.Conversation](ctx, &c.scoped,
		forkConversationCmd{ownerField: ownerField{Owner: c.p.Owner}, ID: id, At: at, Title: title})
}

func (c *conversations) Delete(ctx context.Context, id string) error {
	_, err := dispatch[deleteConversationCmd, struct{}](ctx, &c.scoped,
		deleteConversationCmd{ownerField: ownerField{Owner: c.p.Owner}, ID: id})
	return err
}

// turns implements the Turns interface over dispatched commands.
type turns struct{ scoped }

func (t *turns) Append(ctx context.Context, conversationID string, in AppendTurn) (domain.Turn, error) {
	return dispatch[appendTurnCmd, domain.Turn](ctx, &t.scoped,
		appendTurnCmd{ownerField: ownerField{Owner: t.p.Owner}, ConversationID: conversationID, In: in})
}

func (t *turns) Edit(ctx context.Context, conversationID string, ref domain.TurnRef, in EditTurn) (domain.Turn, error) {
	return dispatch[editTurnCmd, domain.Turn](ctx, &t.scoped,
		editTurnCmd{ownerField: ownerField{Owner: t.p.Owner}, ConversationID: conversationID, Ref: ref, In: in})
}

func (t *turns) Switch(ctx context.Context, conversationID string, ref domain.TurnRef) (domain.Conversation, error) {
	return dispatch[switchTurnCmd, domain.Conversation](ctx, &t.scoped,
		switchTurnCmd{ownerField: ownerField{Owner: t.p.Owner}, ConversationID: conversationID, Ref: ref})
}

func (t *turns) Chain(ctx context.Context, conversationID string, opts store.ChainOptions) ([]domain.Turn, error) {
	return dispatch[chainTurnsCmd, []domain.Turn](ctx, &t.scoped,
		chainTurnsCmd{ownerField: ownerField{Owner: t.p.Owner}, ConversationID: conversationID, Opts: opts})
}

func (t *turns) Branches(ctx context.Context, conversationID string, ref domain.TurnRef) (BranchInfo, error) {
	return dispatch[branchesTurnCmd, BranchInfo](ctx, &t.scoped,
		branchesTurnCmd{ownerField: ownerField{Owner: t.p.Owner}, ConversationID: conversationID, Ref: ref})
}

// runs talks to the run manager directly rather than through the kernel's
// command chain: the manager commits through scoped repositories outside a
// dispatch transaction (see ownerField). It still authorizes the principal,
// and its events carry section 13's names.
type runs struct{ scoped }

func (r *runs) Start(ctx context.Context, conversationID string, in StartRun) (domain.Run, error) {
	if r.p.Owner == "" {
		return domain.Run{}, kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	if conversationID == "" {
		return domain.Run{}, kernel.NewError(kernel.CodeInvalid, "a conversation ID is required")
	}
	if in.Model == "" {
		return domain.Run{}, kernel.NewError(kernel.CodeInvalid, "a model is required")
	}
	opts := r.w.opts
	return r.w.manager.Start(ctx, r.p, conversationID, runOptions(in, opts))
}

func (r *runs) Get(ctx context.Context, runID string) (domain.Run, error) {
	repos, err := r.w.store.Scoped(store.Scope{Owner: r.p.Owner})
	if err != nil {
		return domain.Run{}, kernel.FromStore(err)
	}
	run, err := repos.Runs().Get(ctx, runID)
	return run, kernel.FromStore(err)
}

func (r *runs) Cancel(ctx context.Context, runID string) error {
	if r.p.Owner == "" {
		return kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	return r.w.manager.Cancel(ctx, r.p, runID)
}

func (r *runs) Approve(ctx context.Context, runID, callID string, approve bool, reason string) error {
	if r.p.Owner == "" {
		return kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	return r.w.manager.Approve(ctx, r.p, runID, callID, approve, reason)
}

// content implements the Content interface over dispatched commands.
type contents struct{ scoped }

func (c *contents) Put(ctx context.Context, value domain.Content, baseVersion int) (domain.Content, error) {
	return dispatch[putContentCmd, domain.Content](ctx, &c.scoped,
		putContentCmd{ownerField: ownerField{Owner: c.p.Owner}, In: value, BaseVersion: baseVersion})
}

func (c *contents) Get(ctx context.Context, ref domain.ContentRef) (domain.Content, error) {
	return dispatch[getContentCmd, domain.Content](ctx, &c.scoped,
		getContentCmd{ownerField: ownerField{Owner: c.p.Owner}, Key: ref.Key, Version: ref.Version})
}

func (c *contents) List(ctx context.Context, f store.ContentFilter, p store.PageRequest) (store.Page[domain.Content], error) {
	return dispatch[listContentCmd, store.Page[domain.Content]](ctx, &c.scoped,
		listContentCmd{ownerField: ownerField{Owner: c.p.Owner}, Filter: f, Page: p})
}

func (c *contents) Tombstone(ctx context.Context, key string, baseVersion int) error {
	_, err := dispatch[tombstoneContentCmd, struct{}](ctx, &c.scoped,
		tombstoneContentCmd{ownerField: ownerField{Owner: c.p.Owner}, Key: key, BaseVersion: baseVersion})
	return err
}

// roles implements the Roles interface over dispatched commands.
type roles struct{ scoped }

func (r *roles) Put(ctx context.Context, role domain.Role, expectedRev int64) (domain.Role, error) {
	return dispatch[putRoleCmd, domain.Role](ctx, &r.scoped,
		putRoleCmd{ownerField: ownerField{Owner: r.p.Owner}, In: role, ExpectedRev: expectedRev})
}

func (r *roles) Get(ctx context.Context, id string) (domain.Role, error) {
	return dispatch[getRoleCmd, domain.Role](ctx, &r.scoped,
		getRoleCmd{ownerField: ownerField{Owner: r.p.Owner}, ID: id})
}

func (r *roles) List(ctx context.Context, f store.RoleFilter, p store.PageRequest) (store.Page[domain.Role], error) {
	return dispatch[listRolesCmd, store.Page[domain.Role]](ctx, &r.scoped,
		listRolesCmd{ownerField: ownerField{Owner: r.p.Owner}, Filter: f, Page: p})
}

func (r *roles) Delete(ctx context.Context, id string, expectedRev int64) error {
	_, err := dispatch[deleteRoleCmd, struct{}](ctx, &r.scoped,
		deleteRoleCmd{ownerField: ownerField{Owner: r.p.Owner}, ID: id, ExpectedRev: expectedRev})
	return err
}

// settings implements the Settings interface over dispatched commands.
type settings struct{ scoped }

func (s *settings) Get(ctx context.Context) (domain.Settings, error) {
	return dispatch[getSettingsCmd, domain.Settings](ctx, &s.scoped,
		getSettingsCmd{ownerField: ownerField{Owner: s.p.Owner}})
}

func (s *settings) Update(ctx context.Context, in UpdateSettings) (domain.Settings, error) {
	return dispatch[updateSettingsCmd, domain.Settings](ctx, &s.scoped,
		updateSettingsCmd{ownerField: ownerField{Owner: s.p.Owner}, In: in})
}

// blobs writes bytes to the blob store first, then registers the metadata
// through the kernel's blob.register command, in that order: metadata can
// never point at bytes that are not there (package blob's contract). A crash
// between the two leaves orphan bytes, which the sweep pass removes.
type blobs struct{ scoped }

func (b *blobs) Upload(ctx context.Context, r io.Reader, opts UploadOptions) (domain.Blob, error) {
	if b.p.Owner == "" {
		return domain.Blob{}, kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	info, err := b.w.blobs.Put(ctx, r, blob.PutOptions{MediaType: opts.MediaType})
	if err != nil {
		return domain.Blob{}, kernel.WrapError(kernel.CodeInternal, "storing blob bytes failed", err)
	}
	registered, err := dispatch[registerBlobCmd, domain.Blob](ctx, &b.scoped,
		registerBlobCmd{ownerField: ownerField{Owner: b.p.Owner}, In: domain.Blob{
			ID:        b.w.ids.NewID(),
			Owner:     b.p.Owner,
			Checksum:  info.Checksum,
			Size:      info.Size,
			MediaType: info.MediaType,
			Name:      opts.Name,
		}})
	if err != nil {
		// The bytes are durable but unregistered; the sweep's orphan pass
		// reconciles them. Deleting here would race a concurrent upload of
		// the same content.
		return domain.Blob{}, err
	}
	return registered, nil
}

func (b *blobs) Open(ctx context.Context, id string) (io.ReadSeekCloser, error) {
	repos, err := b.w.store.Scoped(store.Scope{Owner: b.p.Owner})
	if err != nil {
		return nil, kernel.FromStore(err)
	}
	row, err := repos.Blobs().Get(ctx, id)
	if err != nil {
		return nil, kernel.FromStore(err)
	}
	return b.w.blobs.Open(ctx, row.Checksum)
}

// admin is the maintenance surface. It is not a command family, so its
// errors pass through with the store's own detail rather than being forced
// into the kernel's codes.
type admin struct{ w *Workspace }

func (a *admin) Sweep(ctx context.Context) (SweepSummary, error) {
	m, err := a.w.store.Maintenance(store.Scope{System: true})
	if err != nil {
		return SweepSummary{}, err
	}
	opts := store.SweepOptions{Grace: a.w.opts.SweepGrace}
	turns, err := m.SweepTurns(ctx, opts)
	if err != nil {
		return SweepSummary{}, err
	}
	blobs, err := m.SweepBlobs(ctx, opts)
	if err != nil {
		return SweepSummary{}, err
	}
	receipts, err := m.SweepReceipts(ctx, a.w.clock.Now())
	if err != nil {
		return SweepSummary{}, err
	}
	// Byte reconcile: bytes whose checksum has no metadata under any owner,
	// after the same grace the metadata sweeps use (the orphan window
	// between "bytes written" and "metadata registered").
	rec, err := a.w.reconciler.Reconcile(ctx, blob.ReconcileOptions{Grace: a.w.opts.SweepGrace})
	if err != nil {
		return SweepSummary{}, err
	}
	return SweepSummary{
		Turns:    turns.Removed,
		Blobs:    blobs.Removed,
		Receipts: receipts.Removed,
		Bytes:    rec.Removed,
	}, nil
}

func (a *admin) Migrate(ctx context.Context) error {
	_, err := a.w.store.Migrator().Migrate(ctx)
	return err
}

func (a *admin) Health(ctx context.Context) (Health, error) {
	_, capErr := a.w.store.Capabilities(ctx)
	version, verErr := a.w.store.Migrator().SchemaVersion(ctx)
	return Health{
		OK:      capErr == nil && verErr == nil,
		Store:   strings.TrimPrefix(fmt.Sprintf("%T", a.w.store), "*"),
		Version: version,
	}, nil
}

// runOptions maps run.start's payload onto the loop's options, filling the
// limits section 15 gives the workspace.
func runOptions(in StartRun, opts Options) agent.RunOptions {
	return agent.RunOptions{
		Model:         in.Model,
		MaxSteps:      opts.MaxSteps,
		MaxParallel:   opts.MaxParallelTools,
		Deadline:      opts.RunDeadline,
		CostCapMicros: opts.RunCostCapMicros,
		Instructions:  in.Instructions,
		Tools:         in.Tools,
	}
}
