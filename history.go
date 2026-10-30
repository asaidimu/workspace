package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/prompt"
	"github.com/asaidimu/workspace/store"
)

// turnReceiptTTL matches the kernel's receipt TTL: how long a reserved
// append key blocks a retry of the same turn.
const turnReceiptTTL = 24 * time.Hour

// history is the facade's agent.History: the snapshot the pipeline reads and
// the turn commits the loop makes, over owner-scoped repositories. Read takes
// one read-only transaction so a snapshot cannot straddle a commit, and
// Append lands the turn and the head that points at it together.
type history struct {
	store store.Store
	clock domain.Clock
	ids   domain.IDGenerator
}

var _ agent.History = (*history)(nil)

// Read assembles prompt.Snapshot: conversation, role, settings, attached
// content and the active chain, in one read transaction.
func (h *history) Read(ctx context.Context, p kernel.Principal, conversationID string) (prompt.Snapshot, error) {
	if conversationID == "" {
		return prompt.Snapshot{}, kernel.NewError(kernel.CodeInvalid, "a conversation ID is required")
	}
	if p.Owner == "" {
		return prompt.Snapshot{}, kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	var snap prompt.Snapshot
	err := h.store.Atomic(ctx, store.Scope{Owner: p.Owner}, store.TxOptions{ReadOnly: true},
		func(ctx context.Context, tx store.Tx) error {
			conv, err := tx.Conversations().Get(ctx, conversationID)
			if err != nil {
				return err
			}
			snap.Conversation = conv
			snap.Topics = conv.Topics

			settings, err := tx.Settings().Get(ctx)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			snap.Settings = settings

			// The conversation names its role, falling back to the
			// workspace default. A missing role yields no persona rather
			// than failing the build: the pipeline treats a nil role as
			// "no persona section".
			roleID := conv.RoleID
			if roleID == "" {
				roleID = settings.DefaultRoleID
			}
			if roleID != "" {
				role, err := tx.Roles().Get(ctx, roleID)
				switch {
				case err == nil:
					snap.Role = &role
				case errors.Is(err, store.ErrNotFound):
				default:
					return err
				}
			}

			for _, ref := range conv.Attached {
				c, err := tx.Contents().Get(ctx, ref)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				snap.Attached = append(snap.Attached, c)
			}

			// Pre-fetch conversation preference content if not already in snap.Attached.
			attachedKeys := make(map[string]bool, len(snap.Attached))
			for _, c := range snap.Attached {
				attachedKeys[c.Key] = true
			}
			for _, ref := range conv.PreferenceKeys {
				if attachedKeys[ref.Key] {
					continue
				}
				c, err := tx.Contents().Get(ctx, ref)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				snap.Attached = append(snap.Attached, c)
				attachedKeys[c.Key] = true
			}

			if conv.Head != nil {
				chain, err := tx.Turns().Chain(ctx, *conv.Head, store.ChainOptions{})
				if err != nil {
					return err
				}
				// Chain walks from the head down (newest first); the
				// pipeline fits newest first but hands the provider
				// messages oldest first, so the snapshot is the
				// chronological order.
				snap.Transcript = reverse(chain)
			}
			return nil
		})
	if err != nil {
		return prompt.Snapshot{}, kernel.FromStore(err)
	}
	return snap, nil
}

// Append commits one turn and moves the conversation's head to it in the same
// transaction.
func (h *history) Append(ctx context.Context, p kernel.Principal, conversationID string, turn domain.Turn, idempotencyKey string) (domain.Turn, error) {
	if conversationID == "" {
		return domain.Turn{}, kernel.NewError(kernel.CodeInvalid, "a conversation ID is required")
	}
	if p.Owner == "" {
		return domain.Turn{}, kernel.NewError(kernel.CodeForbidden, "an owner is required")
	}
	var committed domain.Turn
	err := h.store.Atomic(ctx, store.Scope{Owner: p.Owner}, store.TxOptions{}, func(ctx context.Context, tx store.Tx) error {
		var err error
		committed, err = commitTurn(ctx, tx, h.clock, h.ids, p, conversationID, turn, idempotencyKey, true)
		return err
	})
	if err != nil {
		return domain.Turn{}, kernel.FromStore(err)
	}
	return committed, nil
}

// commitTurn is the shared write path: identity defaults, idempotency, the
// append and the head move. The command handler calls it inside the kernel's
// transaction; Append above opens its own.
//
// linkToHead is how a linear append differs from a versioned edit: when it is
// set, the turn is filed after whatever the head is at commit time (even if
// the caller's recorded parent has gone stale because someone appended in
// between), and the head follows it. An edit passes false so a new version
// keeps its parent — including nil, which makes it a sibling root version —
// and decides for itself whether the head should follow.
func commitTurn(ctx context.Context, tx store.Tx, clock domain.Clock, ids domain.IDGenerator,
	p kernel.Principal, conversationID string, turn domain.Turn, idempotencyKey string, linkToHead bool) (domain.Turn, error) {
	conv, err := tx.Conversations().Get(ctx, conversationID)
	if err != nil {
		return domain.Turn{}, err
	}

	if idempotencyKey != "" {
		rec, created, err := tx.Receipts().Reserve(ctx, idempotencyKey, turnReceiptTTL)
		if err != nil {
			return domain.Turn{}, err
		}
		if !created {
			if rec.State == domain.ReceiptCompleted {
				var replay domain.Turn
				if err := json.Unmarshal(rec.Result, &replay); err != nil {
					return domain.Turn{}, kernel.WrapError(kernel.CodeInternal, "stored turn receipt does not decode", err)
				}
				return replay, nil
			}
			return domain.Turn{}, kernel.NewError(kernel.CodeConflict, "an identical append is already in flight")
		}
	}

	// The principal's owner is authoritative: the transaction's scope would
	// refuse any other value anyway, and a caller should not be able to file
	// a turn under an owner it never had.
	turn.Owner = p.Owner
	if turn.ID == "" {
		turn.ID = ids.NewID()
	}
	if turn.Version < 1 {
		turn.Version = 1
	}
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = clock.Now()
	}
	// A linear append lands on the active chain. The caller's recorded
	// parent is either absent (the AppendTurn DTO carries none) or stale
	// (a user turn arrived while the loop was mid-step); either way the
	// chain the head points at is where this turn belongs.
	if linkToHead && conv.Head != nil {
		head := *conv.Head
		turn.Parent = &head
	}

	committed, err := tx.Turns().Append(ctx, turn)
	if err != nil {
		return domain.Turn{}, err
	}

	// Move the head only when the new turn extends the chain the head is
	// already on. A turn filed elsewhere (a branch, an edit of an old
	// version) leaves the head for its caller to place.
	extendsHead := conv.Head == nil ||
		(committed.Parent != nil && *committed.Parent == *conv.Head)
	if extendsHead {
		ref := committed.Ref()
		if _, err := tx.Conversations().SetHead(ctx, conversationID, conv.Rev, &ref); err != nil {
			return domain.Turn{}, err
		}
	}

	if idempotencyKey != "" {
		raw, err := json.Marshal(committed)
		if err != nil {
			return domain.Turn{}, kernel.WrapError(kernel.CodeInternal, "committed turn is not JSON-serializable", err)
		}
		if err := tx.Receipts().Complete(ctx, idempotencyKey, raw); err != nil {
			return domain.Turn{}, err
		}
	}
	return committed, nil
}

// reverse flips a newest-first chain into chronological order.
func reverse(turns []domain.Turn) []domain.Turn {
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns
}
