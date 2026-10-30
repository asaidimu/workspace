// Package domain holds the workspace's pure types and rules.
//
// It imports only the standard library. Nothing here knows about a database,
// a provider or a host: the eight invariants in the Workspace specification
// (section 6) are the only behaviour, and they are enforced by the kernel and
// the store adapters, not by this package.
package domain

import (
	"bytes"
	"encoding/json"
	"time"
)

// @note #7wsf3d issue : Lack of schema tags
// @see #ctp1dk
//
// None of these domain types have schema tags, which means
// none of them benefit from anansi's schema utilities such
// as efficient json roundtrips and validators for data in
// either json bytes or map forms.
// Anansi being the intended data layer for this library, this is a very big issue.
// Tags are cheap in that they are metadata and do not import
// anything.
// This omission causes further problems down the line

// Clock is the injected time source. Nothing in the core calls time.Now.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints the identifier for a new record. The production
// implementation is a UUIDv7 generator; tests inject a deterministic one.
type IDGenerator interface {
	NewID() string
}

// TurnRef points at one version of one turn. It is the unit of identity for
// the turn tree: a turn is identified by (ID, Version).
type TurnRef struct {
	ID      string
	Version int
}

// ContentRef points at one version of a content key. Version 0 on a read
// means "the latest non-tombstoned version".
type ContentRef struct {
	Key     string
	Version int
}

// ActorKind is who produced a turn.
type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorAgent  ActorKind = "agent"
	ActorTool   ActorKind = "tool"
	ActorSystem ActorKind = "system"
)

// Actor names the producer of a turn.
type Actor struct {
	Kind ActorKind
	ID   string
}

// Block is one typed piece of a turn or of a content value. Type is an open
// set (the registry's block types); Value is the typed payload and Raw keeps
// the original bytes so an unknown or invalid block survives verbatim.
type Block struct {
	Type  string
	Key   string
	Value any
	Raw   json.RawMessage
}

// Usage records what a model call cost, in the vendor's token counts and in
// micro-KES.
type Usage struct {
	Model        string
	InputTokens  int
	OutputTokens int
	CostMicros   int64
	Currency     string
}

// Turn is an immutable message. Nothing rewrites a turn after it is appended;
// an edit creates a new Version of the same ID.
type Turn struct {
	ID           string
	Version      int
	Owner        string
	Parent       *TurnRef
	Actor        Actor
	Blocks       []Block
	Usage        Usage
	FinishReason string
	CreatedAt    time.Time
}

// Ref returns the turn's identity.
func (t Turn) Ref() TurnRef {
	return TurnRef{ID: t.ID, Version: t.Version}
}

// ContentMode is how a piece of content is exposed to the model.
type ContentMode string

const (
	ModeRead      ContentMode = "read"
	ModeWrite     ContentMode = "write"
	ModeReadWrite ContentMode = "readwrite"
	ModeInternal  ContentMode = "internal"
)

// Content is a keyed, versioned value. Versions are immutable and readers see
// the latest non-tombstoned version.
type Content struct {
	Key        string
	Version    int
	Owner      string
	Type       string
	Mode       ContentMode
	Topics     []string
	Value      json.RawMessage
	Metadata   map[string]string
	Tombstoned bool
	CreatedAt  time.Time
}

// Ref returns the content's identity.
func (c Content) Ref() ContentRef {
	return ContentRef{Key: c.Key, Version: c.Version}
}

// ConversationStatus is the lifecycle state of a conversation.
type ConversationStatus string

const (
	StatusActive   ConversationStatus = "active"
	StatusArchived ConversationStatus = "archived"
)

// Conversation is a titled thread over the turn tree. Its Head is a pointer
// into that tree and the active chain is the parent walk from it.
type Conversation struct {
	ID             string
	Owner          string
	Title          string
	RoleID         string
	Head           *TurnRef
	ForkedFrom     *TurnRef
	Topics         []string
	Attached       []ContentRef
	PreferenceKeys []ContentRef
	Constraints    json.RawMessage
	Status         ConversationStatus
	Rev            int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Role is a persona with per-model constraints.
type Role struct {
	ID             string
	Owner          string
	Name           string
	Persona        string
	PreferenceKeys []ContentRef
	Topics         []string
	Constraints    json.RawMessage
	Rev            int64
}

// RemoteRef is a copy of a blob held by an external provider.
type RemoteRef struct {
	ID        string
	ExpiresAt time.Time
}

// Blob is metadata for a file whose bytes live in the blob store.
type Blob struct {
	ID        string
	Owner     string
	Checksum  string // "sha256:<hex>"
	Size      int64
	MediaType string
	Name      string
	Remotes   map[string]RemoteRef
	CreatedAt time.Time
}

// EntityKind is the sort of record a blob holder link points at.
type EntityKind string

const (
	EntityTurn         EntityKind = "turn"
	EntityContent      EntityKind = "content"
	EntityConversation EntityKind = "conversation"
	EntityRole         EntityKind = "role"
)

// EntityRef names a record that holds a blob, used for holder links and for
// garbage-collection reachability.
type EntityRef struct {
	Kind    EntityKind
	ID      string
	Version int
}

// RunStatus is the lifecycle state of an agent run.
type RunStatus string

const (
	RunQueued  RunStatus = "queued"
	RunRunning RunStatus = "running"
	// RunWaiting is a run parked on a person: an approval, or any pause the
	// loop cannot resolve itself. It is reachable from running and back
	// (section 11: a run only moves forward, except between running and
	// waiting), and a waiting run is not claimable by crash recovery — it is
	// waiting on a decision, not on a worker.
	RunWaiting   RunStatus = "waiting"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// IsTerminal reports whether a status can no longer change.
func (s RunStatus) IsTerminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled
}

// Run is the durable record of one agent execution. State is opaque to the
// store: the loop owns its checkpoint format.
type Run struct {
	ID             string
	ConversationID string
	Status         RunStatus
	Step           int
	State          json.RawMessage
	Worker         string
	HeartbeatAt    time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
	Failure        string
}

// ReceiptState is the lifecycle of an idempotency key.
type ReceiptState string

const (
	ReceiptReserved  ReceiptState = "reserved"
	ReceiptCompleted ReceiptState = "completed"
)

// Receipt is the stored result of an idempotent command.
type Receipt struct {
	Key       string
	State     ReceiptState
	Result    json.RawMessage
	ExpiresAt time.Time
}

// Constraints are the per-model request knobs a role or conversation may
// pin. Every field is optional; a nil pointer means "no opinion". They are
// stored as JSON (Role.Constraints) because roles persist, and are merged
// role -> conversation -> last user turn into one provider request.
type Constraints struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	Stop            []string `json:"stop,omitempty"`
	ThinkingBudget  *int     `json:"thinking_budget,omitempty"`
}

// DecodeConstraints parses a stored constraints JSON document. Empty,
// missing or non-object bytes mean "no constraints"; a malformed object is
// an error the caller treats as configuration damage, not model output.
func DecodeConstraints(raw json.RawMessage) (Constraints, error) {
	var c Constraints
	if len(bytes.TrimSpace(raw)) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, nil
}

// Merge folds b over a: every field the later source sets wins; fields it
// leaves unset keep the earlier source's value. Slice fields replace whole.
func MergeConstraints(a, b Constraints) Constraints {
	out := a
	if b.Temperature != nil {
		out.Temperature = b.Temperature
	}
	if b.MaxOutputTokens != nil {
		out.MaxOutputTokens = b.MaxOutputTokens
	}
	if b.ThinkingBudget != nil {
		out.ThinkingBudget = b.ThinkingBudget
	}
	if len(b.Stop) > 0 {
		out.Stop = append([]string(nil), b.Stop...)
	}
	return out
}

// Settings are the workspace-wide defaults.
type Settings struct {
	Owner                string
	Prompt               string
	Language             string
	DefaultRoleID        string
	GlobalPreferenceKeys []ContentRef
	Rev                  int64
}
