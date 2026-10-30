package workspace

import (
	"encoding/json"

	"github.com/asaidimu/workspace/domain"
)

// CreateConversation is the payload of the conversation.create command.
type CreateConversation struct {
	Title          string
	RoleID         string
	Topics         []string
	Attached       []domain.ContentRef
	PreferenceKeys []domain.ContentRef
	Constraints    json.RawMessage
	Status         domain.ConversationStatus
}

// UpdateConversation is the payload of conversation.update. Nil fields are
// left unchanged.
type UpdateConversation struct {
	Title          *string
	RoleID         *string
	Topics         *[]string
	Attached       *[]domain.ContentRef
	PreferenceKeys *[]domain.ContentRef
	Constraints    *json.RawMessage
	Status         *domain.ConversationStatus
}

// UpdateSettings is the payload of settings.update. Nil fields are left
// unchanged and ExpectedRev is the revision the caller read, so a concurrent
// update conflicts instead of silently overwriting.
type UpdateSettings struct {
	Prompt               *string
	Language             *string
	DefaultRoleID        *string
	GlobalPreferenceKeys *[]domain.ContentRef
	ExpectedRev          int64
}

// AppendTurn is the payload of turn.append.
type AppendTurn struct {
	Actor        domain.Actor
	Blocks       []domain.Block
	Usage        domain.Usage
	FinishReason string
}

// EditTurn is the payload of turn.edit: a new version of an existing turn
// ID, sharing the original's parent.
type EditTurn struct {
	Actor        domain.Actor
	Blocks       []domain.Block
	Usage        domain.Usage
	FinishReason string
}

// StartRun is the payload of run.start.
type StartRun struct {
	Model        string
	MaxSteps     int
	MaxParallel  int
	Instructions string
	Tools        []string
}
