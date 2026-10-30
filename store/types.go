package store

import (
	"encoding/json"
	"time"

	"github.com/asaidimu/workspace/domain"
)

// TurnRef points at one version of one turn. It is re-exported here so the
// store port is self-contained for callers that only touch persistence; the
// canonical definition is in domain.
type TurnRef = domain.TurnRef

// ContentRef points at one version of a content key. Version 0 on a read
// means the latest non-tombstoned version.
type ContentRef = domain.ContentRef

// ConversationFilter narrows a conversation listing.
type ConversationFilter struct {
	IDs          []string
	Topics       TopicFilter
	RoleID       string
	Status       string
	UpdatedAfter *time.Time
}

// ConversationPatch is the mutable part of a conversation. A nil field is
// left unchanged.
type ConversationPatch struct {
	Title          *string
	RoleID         *string
	Topics         *[]string
	Attached       *[]domain.ContentRef
	PreferenceKeys *[]domain.ContentRef
	Constraints    *json.RawMessage
	Status         *domain.ConversationStatus
}

// ContentFilter narrows a content listing.
type ContentFilter struct {
	Keys              []string
	Types             []string
	Modes             []string
	Topics            TopicFilter
	AllVersions       bool
	IncludeTombstoned bool
}

// RoleFilter narrows a role listing.
type RoleFilter struct {
	IDs    []string
	Topics TopicFilter
}

// ChainOptions bounds a parent walk from a head. Limit keeps only the
// newest Limit turns; Until stops before that ancestor (exclusive).
type ChainOptions struct {
	Limit int
	Until *domain.TurnRef
}

// MigrationReport is what one migration pass applied.
type MigrationReport struct {
	From    int
	To      int
	Applied int
	Refused bool
	Refusal string
}
