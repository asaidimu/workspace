package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/store"
)

// ownerField is embedded by every dispatched command. A handler needs the
// principal's owner to stamp the rows it creates, and the kernel's handler
// signature carries no principal — the transaction's scope was built from
// that same owner, so a command whose Owner disagreed with the dispatching
// principal would be refused by the store anyway.
//
// Run commands are the one family that does not dispatch (section 13's
// run.start, run.cancel, run.approve): the manager commits through scoped
// repositories outside any dispatch transaction, and a write from inside a
// handler's transaction would be lost when that transaction commits. Their
// events still carry the specification's names because the manager emits
// them itself.
type ownerField struct {
	Owner string
}

// Conversation commands.

type createConversationCmd struct {
	ownerField
	In CreateConversation
}

func (createConversationCmd) CommandName() string { return "conversation.create" }

type getConversationCmd struct {
	ownerField
	ID string
}

func (getConversationCmd) CommandName() string { return "conversation.get" }
func (c getConversationCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ID}
}

type listConversationsCmd struct {
	ownerField
	Filter store.ConversationFilter
	Page   store.PageRequest
}

func (listConversationsCmd) CommandName() string { return "conversation.list" }

type updateConversationCmd struct {
	ownerField
	ID string
	In UpdateConversation
}

func (updateConversationCmd) CommandName() string { return "conversation.update" }
func (c updateConversationCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ID}
}

type deleteConversationCmd struct {
	ownerField
	ID string
}

func (deleteConversationCmd) CommandName() string { return "conversation.delete" }
func (c deleteConversationCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ID}
}

type forkConversationCmd struct {
	ownerField
	ID    string
	At    domain.TurnRef
	Title string
}

func (forkConversationCmd) CommandName() string { return "conversation.fork" }
func (c forkConversationCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ID}
}

// Turn commands. Their target is the conversation, so their events are
// ordered and filtered with it (kernel.publish reads Target).

type appendTurnCmd struct {
	ownerField
	ConversationID string
	In             AppendTurn
}

func (appendTurnCmd) CommandName() string { return "turn.append" }
func (c appendTurnCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ConversationID}
}

type editTurnCmd struct {
	ownerField
	ConversationID string
	Ref            domain.TurnRef
	In             EditTurn
}

func (editTurnCmd) CommandName() string { return "turn.edit" }
func (c editTurnCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ConversationID}
}

type switchTurnCmd struct {
	ownerField
	ConversationID string
	Ref            domain.TurnRef
}

func (switchTurnCmd) CommandName() string { return "turn.switch" }
func (c switchTurnCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ConversationID}
}

type chainTurnsCmd struct {
	ownerField
	ConversationID string
	Opts           store.ChainOptions
}

func (chainTurnsCmd) CommandName() string { return "turn.chain" }
func (c chainTurnsCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ConversationID}
}

type branchesTurnCmd struct {
	ownerField
	ConversationID string
	Ref            domain.TurnRef
}

func (branchesTurnCmd) CommandName() string { return "turn.branches" }
func (c branchesTurnCmd) Target() kernel.Target {
	return kernel.Target{Kind: "conversation", ID: c.ConversationID}
}

// Role commands.

type putRoleCmd struct {
	ownerField
	In          domain.Role
	ExpectedRev int64
}

func (putRoleCmd) CommandName() string { return "role.put" }
func (c putRoleCmd) Target() kernel.Target {
	return kernel.Target{Kind: "role", ID: c.In.ID}
}

type getRoleCmd struct {
	ownerField
	ID string
}

func (getRoleCmd) CommandName() string     { return "role.get" }
func (c getRoleCmd) Target() kernel.Target { return kernel.Target{Kind: "role", ID: c.ID} }

type listRolesCmd struct {
	ownerField
	Filter store.RoleFilter
	Page   store.PageRequest
}

func (listRolesCmd) CommandName() string { return "role.list" }

type deleteRoleCmd struct {
	ownerField
	ID          string
	ExpectedRev int64
}

func (deleteRoleCmd) CommandName() string     { return "role.delete" }
func (c deleteRoleCmd) Target() kernel.Target { return kernel.Target{Kind: "role", ID: c.ID} }

// Content commands.

type putContentCmd struct {
	ownerField
	In          domain.Content
	BaseVersion int
}

func (putContentCmd) CommandName() string { return "content.put" }
func (c putContentCmd) Target() kernel.Target {
	return kernel.Target{Kind: "content", ID: c.In.Key}
}

type getContentCmd struct {
	ownerField
	Key     string
	Version int
}

func (getContentCmd) CommandName() string { return "content.get" }
func (c getContentCmd) Target() kernel.Target {
	return kernel.Target{Kind: "content", ID: c.Key}
}

type listContentCmd struct {
	ownerField
	Filter store.ContentFilter
	Page   store.PageRequest
}

func (listContentCmd) CommandName() string { return "content.list" }

type tombstoneContentCmd struct {
	ownerField
	Key         string
	BaseVersion int
}

func (tombstoneContentCmd) CommandName() string { return "content.tombstone" }
func (c tombstoneContentCmd) Target() kernel.Target {
	return kernel.Target{Kind: "content", ID: c.Key}
}

// Blob commands. blob.register is section 13's command: the bytes are
// already in the blob store when it runs, and the handler only records the
// metadata and its holder links.
type registerBlobCmd struct {
	ownerField
	In domain.Blob
}

func (registerBlobCmd) CommandName() string { return "blob.register" }
func (c registerBlobCmd) Target() kernel.Target {
	return kernel.Target{Kind: "blob", ID: c.In.Checksum}
}

// Settings commands. Settings are the workspace-wide defaults of section 13;
// settings.get is a read through the command pipeline so it is authorized
// and observable like every other read.

type getSettingsCmd struct {
	ownerField
}

func (getSettingsCmd) CommandName() string { return "settings.get" }

type updateSettingsCmd struct {
	ownerField
	In UpdateSettings
}

func (updateSettingsCmd) CommandName() string { return "settings.update" }

// Events. Each matches section 13's name for the command that produced it.
// Handlers return them only after their writes have succeeded; the kernel
// publishes them after the transaction commits.

type conversationCreated struct {
	ID string
}

func (conversationCreated) EventName() string { return "conversation.created" }

type conversationUpdated struct {
	ID  string
	Rev int64
}

func (conversationUpdated) EventName() string { return "conversation.updated" }

type settingsUpdated struct {
	Rev int64
}

func (settingsUpdated) EventName() string { return "settings.updated" }

type conversationDeleted struct {
	ID string
}

func (conversationDeleted) EventName() string { return "conversation.deleted" }

type conversationHeadMoved struct {
	ConversationID string
	Head           domain.TurnRef
}

func (conversationHeadMoved) EventName() string { return "conversation.head_moved" }

type turnAppended struct {
	ConversationID string
	Turn           domain.TurnRef
}

func (turnAppended) EventName() string { return "turn.appended" }

type roleChanged struct {
	ID      string
	Rev     int64
	Deleted bool
}

func (roleChanged) EventName() string { return "role.changed" }

type contentChanged struct {
	Key        string
	Version    int
	Tombstoned bool
}

func (contentChanged) EventName() string { return "content.changed" }

type blobRegistered struct {
	ID       string
	Checksum string
	Created  bool
}

func (blobRegistered) EventName() string { return "blob.registered" }

// registerCommands installs every handler on the kernel. It runs once, from
// Open, and a duplicate name fails the open rather than the first dispatch.
func (w *Workspace) registerCommands() error {
	for _, reg := range []func() error{
		func() error {
			return kernel.Register[createConversationCmd, domain.Conversation](w.k, w.createConversation)
		},
		func() error {
			return kernel.Register[getConversationCmd, domain.Conversation](w.k, w.getConversation)
		},
		func() error {
			return kernel.Register[listConversationsCmd, store.Page[domain.Conversation]](w.k, w.listConversations)
		},
		func() error {
			return kernel.Register[updateConversationCmd, domain.Conversation](w.k, w.updateConversation)
		},
		func() error {
			return kernel.Register[deleteConversationCmd, struct{}](w.k, w.deleteConversation)
		},
		func() error {
			return kernel.Register[forkConversationCmd, domain.Conversation](w.k, w.forkConversation)
		},
		func() error {
			return kernel.Register[appendTurnCmd, domain.Turn](w.k, w.appendTurn)
		},
		func() error {
			return kernel.Register[editTurnCmd, domain.Turn](w.k, w.editTurn)
		},
		func() error {
			return kernel.Register[switchTurnCmd, domain.Conversation](w.k, w.switchTurn)
		},
		func() error {
			return kernel.Register[chainTurnsCmd, []domain.Turn](w.k, w.chainTurns)
		},
		func() error {
			return kernel.Register[branchesTurnCmd, BranchInfo](w.k, w.branchesTurn)
		},
		func() error {
			return kernel.Register[putRoleCmd, domain.Role](w.k, w.putRole)
		},
		func() error {
			return kernel.Register[getRoleCmd, domain.Role](w.k, w.getRole)
		},
		func() error {
			return kernel.Register[listRolesCmd, store.Page[domain.Role]](w.k, w.listRoles)
		},
		func() error {
			return kernel.Register[deleteRoleCmd, struct{}](w.k, w.deleteRole)
		},
		func() error {
			return kernel.Register[putContentCmd, domain.Content](w.k, w.putContent)
		},
		func() error {
			return kernel.Register[getContentCmd, domain.Content](w.k, w.getContent)
		},
		func() error {
			return kernel.Register[listContentCmd, store.Page[domain.Content]](w.k, w.listContent)
		},
		func() error {
			return kernel.Register[tombstoneContentCmd, struct{}](w.k, w.tombstoneContent)
		},
		func() error {
			return kernel.Register[registerBlobCmd, domain.Blob](w.k, w.registerBlob)
		},
		func() error {
			return kernel.Register[getSettingsCmd, domain.Settings](w.k, w.getSettings)
		},
		func() error {
			return kernel.Register[updateSettingsCmd, domain.Settings](w.k, w.updateSettings)
		},
	} {
		if err := reg(); err != nil {
			return err
		}
	}
	return nil
}

// --- conversation handlers ---

func (w *Workspace) createConversation(ctx context.Context, tx store.Tx, c createConversationCmd) (domain.Conversation, []kernel.Event, error) {
	if err := roleExists(ctx, tx, c.In.RoleID); err != nil {
		return domain.Conversation{}, nil, err
	}
	conv := domain.Conversation{
		ID:             w.ids.NewID(),
		Owner:          c.Owner,
		Title:          c.In.Title,
		RoleID:         c.In.RoleID,
		Topics:         c.In.Topics,
		Attached:       c.In.Attached,
		PreferenceKeys: c.In.PreferenceKeys,
		Constraints:    c.In.Constraints,
		Status:         c.In.Status,
	}
	created, err := tx.Conversations().Create(ctx, conv)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	return created, []kernel.Event{conversationCreated{ID: created.ID}}, nil
}

func (w *Workspace) getConversation(ctx context.Context, tx store.Tx, c getConversationCmd) (domain.Conversation, []kernel.Event, error) {
	conv, err := tx.Conversations().Get(ctx, c.ID)
	return conv, nil, err
}

func (w *Workspace) listConversations(ctx context.Context, tx store.Tx, c listConversationsCmd) (store.Page[domain.Conversation], []kernel.Event, error) {
	page, err := tx.Conversations().List(ctx, c.Filter, c.Page)
	return page, nil, err
}

func (w *Workspace) updateConversation(ctx context.Context, tx store.Tx, c updateConversationCmd) (domain.Conversation, []kernel.Event, error) {
	conv, err := tx.Conversations().Get(ctx, c.ID)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	if c.In.RoleID != nil {
		if err := roleExists(ctx, tx, *c.In.RoleID); err != nil {
			return domain.Conversation{}, nil, err
		}
	}
	patch := store.ConversationPatch{
		Title:          c.In.Title,
		RoleID:         c.In.RoleID,
		Topics:         c.In.Topics,
		Attached:       c.In.Attached,
		PreferenceKeys: c.In.PreferenceKeys,
		Constraints:    c.In.Constraints,
		Status:         c.In.Status,
	}
	updated, err := tx.Conversations().Update(ctx, c.ID, conv.Rev, patch)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	return updated, []kernel.Event{conversationUpdated{ID: updated.ID, Rev: updated.Rev}}, nil
}

func (w *Workspace) deleteConversation(ctx context.Context, tx store.Tx, c deleteConversationCmd) (struct{}, []kernel.Event, error) {
	conv, err := tx.Conversations().Get(ctx, c.ID)
	if err != nil {
		return struct{}{}, nil, err
	}
	if err := tx.Conversations().Delete(ctx, c.ID, conv.Rev); err != nil {
		return struct{}{}, nil, err
	}
	return struct{}{}, []kernel.Event{conversationDeleted{ID: c.ID}}, nil
}

// forkConversation writes one row that points into the shared turn tree, so
// a fork costs one insert and copies no history (section 3, faithful
// history).
func (w *Workspace) forkConversation(ctx context.Context, tx store.Tx, c forkConversationCmd) (domain.Conversation, []kernel.Event, error) {
	src, err := tx.Conversations().Get(ctx, c.ID)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	if _, err := tx.Turns().Get(ctx, c.At); err != nil {
		return domain.Conversation{}, nil, err
	}
	title := c.Title
	if title == "" {
		title = src.Title
	}
	at := c.At
	fork := domain.Conversation{
		ID:         w.ids.NewID(),
		Owner:      c.Owner,
		Title:      title,
		RoleID:     src.RoleID,
		Head:       &at,
		ForkedFrom: &at,
		Topics:     src.Topics,
		Attached:   src.Attached,
		Status:     src.Status,
	}
	created, err := tx.Conversations().Create(ctx, fork)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	return created, []kernel.Event{conversationCreated{ID: created.ID}}, nil
}

// --- turn handlers ---

// validateBlocks checks each block against the registry: the type must be
// registered and the payload must validate strictly. The caller's payload
// map is kept as-is — the prompt pipeline reads maps and the store retains
// what was sent; validation gates the write, it does not rebind it.
func (w *Workspace) validateBlocks(blocks []domain.Block) error {
	for i, b := range blocks {
		t, ok := w.blocks.Lookup(b.Type)
		if !ok {
			return kernel.NewError(kernel.CodeInvalid,
				fmt.Sprintf("block %d has unknown type %q", i, b.Type))
		}
		payload, err := blockPayload(b)
		if err != nil {
			return kernel.NewError(kernel.CodeInvalid,
				fmt.Sprintf("block %d (%s) payload is not an object", i, b.Type))
		}
		value, issues := t.Decode(payload)
		if value == nil {
			return kernel.NewError(kernel.CodeInvalid,
				fmt.Sprintf("block %d (%s) is invalid", i, b.Type)).WithIssues(issues)
		}
	}
	return nil
}

// blockPayload returns the block's payload as an object, preferring the
// decoded Value and falling back to the original bytes.
func blockPayload(b domain.Block) (map[string]any, error) {
	if m, ok := b.Value.(map[string]any); ok {
		return m, nil
	}
	if b.Value == nil && len(b.Raw) == 0 {
		return map[string]any{}, nil
	}
	raw := b.Raw
	if len(raw) == 0 {
		var err error
		if raw, err = json.Marshal(b.Value); err != nil {
			return nil, err
		}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// appendTurn is section 13's "add a user or system turn": the agent's own
// turns arrive through History, not through this command.
func (w *Workspace) appendTurn(ctx context.Context, tx store.Tx, c appendTurnCmd) (domain.Turn, []kernel.Event, error) {
	if c.In.Actor.Kind != domain.ActorUser && c.In.Actor.Kind != domain.ActorSystem {
		return domain.Turn{}, nil, kernel.NewError(kernel.CodeInvalid, "turn.append takes a user or system turn")
	}
	if len(c.In.Blocks) == 0 {
		return domain.Turn{}, nil, kernel.NewError(kernel.CodeInvalid, "a turn needs at least one block")
	}
	if err := w.validateBlocks(c.In.Blocks); err != nil {
		return domain.Turn{}, nil, err
	}
	turn := domain.Turn{
		Actor:        c.In.Actor,
		Blocks:       c.In.Blocks,
		Usage:        c.In.Usage,
		FinishReason: c.In.FinishReason,
	}
	committed, err := commitTurn(ctx, tx, w.clock, w.ids, kernel.Principal{Owner: c.Owner}, c.ConversationID, turn, "", true)
	if err != nil {
		return domain.Turn{}, nil, err
	}
	return committed, []kernel.Event{turnAppended{ConversationID: c.ConversationID, Turn: committed.Ref()}}, nil
}

// editTurn files a new Version of an existing turn ID under the same parent;
// nothing the turn said is rewritten. When the edited version is the head,
// the head follows it.
func (w *Workspace) editTurn(ctx context.Context, tx store.Tx, c editTurnCmd) (domain.Turn, []kernel.Event, error) {
	old, err := tx.Turns().Get(ctx, c.Ref)
	if err != nil {
		return domain.Turn{}, nil, err
	}
	if len(c.In.Blocks) == 0 {
		return domain.Turn{}, nil, kernel.NewError(kernel.CodeInvalid, "a turn needs at least one block")
	}
	if err := w.validateBlocks(c.In.Blocks); err != nil {
		return domain.Turn{}, nil, err
	}
	actor := c.In.Actor
	if actor.Kind == "" {
		actor = old.Actor
	}
	usage := c.In.Usage
	if usage == (domain.Usage{}) {
		usage = old.Usage
	}
	finish := c.In.FinishReason
	if finish == "" {
		finish = old.FinishReason
	}
	edited := domain.Turn{
		ID:           old.ID,
		Version:      old.Version + 1,
		Parent:       old.Parent,
		Actor:        actor,
		Blocks:       c.In.Blocks,
		Usage:        usage,
		FinishReason: finish,
		CreatedAt:    w.clock.Now(),
	}
	committed, err := commitTurn(ctx, tx, w.clock, w.ids, kernel.Principal{Owner: c.Owner}, c.ConversationID, edited, "", false)
	if err != nil {
		return domain.Turn{}, nil, err
	}

	conv, err := tx.Conversations().Get(ctx, c.ConversationID)
	if err != nil {
		return domain.Turn{}, nil, err
	}
	if conv.Head != nil && *conv.Head == c.Ref {
		ref := committed.Ref()
		if _, err := tx.Conversations().SetHead(ctx, c.ConversationID, conv.Rev, &ref); err != nil {
			return domain.Turn{}, nil, err
		}
	}
	return committed, []kernel.Event{turnAppended{ConversationID: c.ConversationID, Turn: committed.Ref()}}, nil
}

// switchTurn moves the head to an existing turn, which is how a caller
// selects another branch. The tree is untouched: only the pointer moves.
func (w *Workspace) switchTurn(ctx context.Context, tx store.Tx, c switchTurnCmd) (domain.Conversation, []kernel.Event, error) {
	conv, err := tx.Conversations().Get(ctx, c.ConversationID)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	if _, err := tx.Turns().Get(ctx, c.Ref); err != nil {
		return domain.Conversation{}, nil, err
	}
	moved, err := tx.Conversations().SetHead(ctx, c.ConversationID, conv.Rev, &c.Ref)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	return moved, []kernel.Event{conversationHeadMoved{ConversationID: c.ConversationID, Head: c.Ref}}, nil
}

// chainTurns walks the active chain from the head. The store walks newest
// first; the Handle returns the chain oldest first, the order a reader walks
// a conversation.
func (w *Workspace) chainTurns(ctx context.Context, tx store.Tx, c chainTurnsCmd) ([]domain.Turn, []kernel.Event, error) {
	conv, err := tx.Conversations().Get(ctx, c.ConversationID)
	if err != nil {
		return nil, nil, err
	}
	if conv.Head == nil {
		return []domain.Turn{}, nil, nil
	}
	chain, err := tx.Turns().Chain(ctx, *conv.Head, c.Opts)
	if err != nil {
		return nil, nil, err
	}
	return reverse(chain), nil, nil
}

// branchesTurn reports the sibling versions of a turn and which one the
// conversation currently reads.
func (w *Workspace) branchesTurn(ctx context.Context, tx store.Tx, c branchesTurnCmd) (BranchInfo, []kernel.Event, error) {
	conv, err := tx.Conversations().Get(ctx, c.ConversationID)
	if err != nil {
		return BranchInfo{}, nil, err
	}
	if _, err := tx.Turns().Get(ctx, c.Ref); err != nil {
		return BranchInfo{}, nil, err
	}
	siblings, err := tx.Turns().Versions(ctx, c.Ref.ID)
	if err != nil {
		return BranchInfo{}, nil, err
	}
	active := c.Ref
	if conv.Head != nil && conv.Head.ID == c.Ref.ID {
		active = *conv.Head
	}
	return BranchInfo{Active: active, Siblings: siblings}, nil, nil
}

// --- role handlers ---

func (w *Workspace) putRole(ctx context.Context, tx store.Tx, c putRoleCmd) (domain.Role, []kernel.Event, error) {
	role := c.In
	role.Owner = c.Owner
	if role.ID == "" {
		role.ID = w.ids.NewID()
	}
	if role.Name == "" {
		return domain.Role{}, nil, kernel.NewError(kernel.CodeInvalid, "a role needs a name")
	}
	saved, err := tx.Roles().Put(ctx, role, c.ExpectedRev)
	if err != nil {
		return domain.Role{}, nil, err
	}
	return saved, []kernel.Event{roleChanged{ID: saved.ID, Rev: saved.Rev}}, nil
}

func (w *Workspace) getRole(ctx context.Context, tx store.Tx, c getRoleCmd) (domain.Role, []kernel.Event, error) {
	role, err := tx.Roles().Get(ctx, c.ID)
	return role, nil, err
}

func (w *Workspace) listRoles(ctx context.Context, tx store.Tx, c listRolesCmd) (store.Page[domain.Role], []kernel.Event, error) {
	page, err := tx.Roles().List(ctx, c.Filter, c.Page)
	return page, nil, err
}

func (w *Workspace) deleteRole(ctx context.Context, tx store.Tx, c deleteRoleCmd) (struct{}, []kernel.Event, error) {
	if err := tx.Roles().Delete(ctx, c.ID, c.ExpectedRev); err != nil {
		return struct{}{}, nil, err
	}
	return struct{}{}, []kernel.Event{roleChanged{ID: c.ID, Rev: c.ExpectedRev + 1, Deleted: true}}, nil
}

// --- content handlers ---

func (w *Workspace) putContent(ctx context.Context, tx store.Tx, c putContentCmd) (domain.Content, []kernel.Event, error) {
	value := c.In
	value.Owner = c.Owner
	if value.Key == "" {
		value.Key = w.ids.NewID()
	}
	if len(value.Value) == 0 {
		return domain.Content{}, nil, kernel.NewError(kernel.CodeInvalid, "content needs a value")
	}
	saved, err := tx.Contents().Put(ctx, value, c.BaseVersion)
	if err != nil {
		return domain.Content{}, nil, err
	}
	return saved, []kernel.Event{contentChanged{Key: saved.Key, Version: saved.Version}}, nil
}

func (w *Workspace) getContent(ctx context.Context, tx store.Tx, c getContentCmd) (domain.Content, []kernel.Event, error) {
	content, err := tx.Contents().Get(ctx, domain.ContentRef{Key: c.Key, Version: c.Version})
	return content, nil, err
}

func (w *Workspace) listContent(ctx context.Context, tx store.Tx, c listContentCmd) (store.Page[domain.Content], []kernel.Event, error) {
	page, err := tx.Contents().List(ctx, c.Filter, c.Page)
	return page, nil, err
}

func (w *Workspace) tombstoneContent(ctx context.Context, tx store.Tx, c tombstoneContentCmd) (struct{}, []kernel.Event, error) {
	if err := tx.Contents().Tombstone(ctx, c.Key, c.BaseVersion); err != nil {
		return struct{}{}, nil, err
	}
	return struct{}{}, []kernel.Event{contentChanged{Key: c.Key, Version: c.BaseVersion, Tombstoned: true}}, nil
}

// roleExists refuses a conversation that names a role which does not exist,
// so a dangling persona cannot silently mean "no persona".
func roleExists(ctx context.Context, tx store.Tx, roleID string) error {
	if roleID == "" {
		return nil
	}
	if _, err := tx.Roles().Get(ctx, roleID); err != nil {
		if isNotFound(err) {
			return kernel.NewError(kernel.CodeInvalid, "conversation names a role that does not exist")
		}
		return err
	}
	return nil
}

// registerBlob records bytes that already exist in the blob store. A second
// registration of the same checksum reports the existing row instead of
// failing: blob storage is content-addressed, so the same bytes are the same
// blob.
func (w *Workspace) registerBlob(ctx context.Context, tx store.Tx, c registerBlobCmd) (domain.Blob, []kernel.Event, error) {
	blob := c.In
	blob.Owner = c.Owner
	if blob.ID == "" {
		blob.ID = w.ids.NewID()
	}
	if blob.Checksum == "" {
		return domain.Blob{}, nil, kernel.NewError(kernel.CodeInvalid, "a blob needs a checksum")
	}
	saved, created, err := tx.Blobs().Register(ctx, blob)
	if err != nil {
		return domain.Blob{}, nil, err
	}
	return saved, []kernel.Event{blobRegistered{ID: saved.ID, Checksum: saved.Checksum, Created: created}}, nil
}

// --- settings handlers ---

func (w *Workspace) getSettings(ctx context.Context, tx store.Tx, c getSettingsCmd) (domain.Settings, []kernel.Event, error) {
	s, err := tx.Settings().Get(ctx)
	return s, nil, err
}

// updateSettings applies the patch to the current settings and writes it back
// with the caller's expected revision. A workspace that has never written
// settings reads as absent; the patch then builds them from the defaults with
// the caller's revision 0, and Put creates the row.
func (w *Workspace) updateSettings(ctx context.Context, tx store.Tx, c updateSettingsCmd) (domain.Settings, []kernel.Event, error) {
	s := domain.Settings{Owner: c.Owner}
	cur, err := tx.Settings().Get(ctx)
	switch {
	case err == nil:
		s = cur
	case isNotFound(err):
		// First write: defaults plus the patch.
	default:
		return domain.Settings{}, nil, err
	}
	if c.In.Prompt != nil {
		s.Prompt = *c.In.Prompt
	}
	if c.In.Language != nil {
		s.Language = *c.In.Language
	}
	if c.In.DefaultRoleID != nil {
		s.DefaultRoleID = *c.In.DefaultRoleID
	}
	if c.In.GlobalPreferenceKeys != nil {
		s.GlobalPreferenceKeys = append([]domain.ContentRef(nil), (*c.In.GlobalPreferenceKeys)...)
	}
	s.Owner = c.Owner
	saved, err := tx.Settings().Put(ctx, s, c.In.ExpectedRev)
	if err != nil {
		return domain.Settings{}, nil, err
	}
	return saved, []kernel.Event{settingsUpdated{Rev: saved.Rev}}, nil
}

// isNotFound reports whether err is the store's not-found sentinel. Handler
// bodies return store errors untouched — the kernel's dispatcher classifies
// them on the way out — so this only serves code paths that must branch on
// absence inside a handler.
func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
