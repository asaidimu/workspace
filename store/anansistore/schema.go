package anansistore

import (
	"fmt"

	"github.com/asaidimu/go-anansi/v8/core/data"
	"github.com/asaidimu/go-anansi/v8/core/schema/definition"
)

// The internal shape structs are the single source of truth for collection
// schemas: field ids are minted as UUIDv7 by data.SchemaFrom (production-valid
// without ANANSI_ENV), names come from the anansi tags, and the same names are
// the keys the repos write into data.NewDocument maps. "unique" is not part
// of the tag grammar, so identity fields are flagged after FromJSON.
//
// Only identity/owner fields are required=true: the port validates exactly
// what the memory store validates (ID + scope), and empty-but-present
// strings must pass schema validation. Optional data is nullable/absent —
// round-tripping nil vs empty is done with HasKey, not with "" sentinels.

type conversationShape struct {
	CID                string `anansi:"conversation_id,required=true"`
	Owner              string `anansi:"owner,required=true"`
	Title              string `anansi:"title"`
	RoleID             string `anansi:"role_id"`
	HeadKey            string `anansi:"head_key"`
	ForkedFrom         string `anansi:"forked_from_key"`
	TopicsJSON         string `anansi:"topics_json"`
	AttachedJSON       string `anansi:"attached_json"`
	PreferenceKeysJSON string `anansi:"preference_keys_json"`
	Constraints        string `anansi:"constraints"`
	Status             string `anansi:"status"`
	CreatedAt          string `anansi:"created_at"`
	UpdatedAt          string `anansi:"updated_at"`
}

type turnShape struct {
	TurnKey      string `anansi:"turn_key,required=true"`
	TurnID       string `anansi:"turn_id,required=true"`
	TurnVersion  int    `anansi:"turn_version"`
	Owner        string `anansi:"owner,required=true"`
	ParentKey    string `anansi:"parent_key"`
	ActorKind    string `anansi:"actor_kind"`
	ActorID      string `anansi:"actor_id"`
	BlocksJSON   string `anansi:"blocks_json"`
	UsageJSON    string `anansi:"usage_json"`
	FinishReason string `anansi:"finish_reason"`
	CreatedAt    string `anansi:"created_at"`
}

type contentShape struct {
	ContentKey  string `anansi:"content_key,required=true"`
	ContentID   string `anansi:"content_id,required=true"`
	Version     int    `anansi:"version"`
	Owner       string `anansi:"owner,required=true"`
	ContentType string `anansi:"content_type"`
	Mode        string `anansi:"mode"`
	TopicsJSON  string `anansi:"topics_json"`
	Value       string `anansi:"value"`
	MetaJSON    string `anansi:"meta_json"`
	Tombstoned  bool   `anansi:"tombstoned"`
	CreatedAt   string `anansi:"created_at"`
}

type roleShape struct {
	RoleID             string `anansi:"role_id,required=true"`
	Owner              string `anansi:"owner,required=true"`
	Name               string `anansi:"name"`
	Persona            string `anansi:"persona"`
	PreferenceKeysJSON string `anansi:"preference_keys_json"`
	TopicsJSON         string `anansi:"topics_json"`
	Constraints        string `anansi:"constraints"`
}

type blobShape struct {
	BlobID      string `anansi:"blob_id,required=true"`
	Owner       string `anansi:"owner,required=true"`
	Checksum    string `anansi:"checksum,required=true"`
	Size        int    `anansi:"size_bytes"`
	MediaType   string `anansi:"media_type"`
	Name        string `anansi:"blob_name"`
	RemotesJSON string `anansi:"remotes_json"`
	CreatedAt   string `anansi:"created_at"`
}

type blobRefShape struct {
	RefKey        string `anansi:"ref_key,required=true"`
	BlobID        string `anansi:"blob_id,required=true"`
	Kind          string `anansi:"kind"`
	EntityID      string `anansi:"entity_id"`
	EntityVersion int    `anansi:"entity_version"`
}

type runShape struct {
	RunID          string `anansi:"run_id,required=true"`
	ConversationID string `anansi:"conversation_id,required=true"`
	Status         string `anansi:"status"`
	Step           int    `anansi:"step"`
	State          string `anansi:"state"`
	Worker         string `anansi:"worker"`
	HeartbeatAt    string `anansi:"heartbeat_at"`
	StartedAt      string `anansi:"started_at"`
	FinishedAt     string `anansi:"finished_at"`
	Failure        string `anansi:"failure"`
}

type receiptShape struct {
	ReceiptKey string `anansi:"receipt_key,required=true"`
	State      string `anansi:"state"`
	Result     string `anansi:"result"`
	ExpiresAt  string `anansi:"expires_at"`
}

type settingsShape struct {
	SettingsKey     string `anansi:"settings_key,required=true"`
	Owner           string `anansi:"owner"`
	Prompt          string `anansi:"prompt"`
	Language        string `anansi:"language"`
	DefaultRoleID   string `anansi:"default_role_id"`
	GlobalPrefsJSON string `anansi:"global_prefs_json"`
}

// collectionSpec names one collection: the suffix appended to Options.Prefix
// and the fields that must carry a UNIQUE column constraint (there is no
// "unique" tag key in the schema extractor, so it is applied post-parse).
type collectionSpec struct {
	suffix string
	unique []string
}

var collectionSpecs = []collectionSpec{
	{suffix: "conversations", unique: []string{"conversation_id"}},
	{suffix: "turns", unique: []string{"turn_key"}},
	{suffix: "contents", unique: []string{"content_key"}},
	{suffix: "roles", unique: []string{"role_id"}},
	{suffix: "blobs", unique: []string{"blob_id"}},
	{suffix: "blob_refs", unique: []string{"ref_key"}},
	{suffix: "runs", unique: []string{"run_id"}},
	{suffix: "receipts", unique: []string{"receipt_key"}},
	{suffix: "settings", unique: []string{"settings_key"}},
}

// deriveSchema builds the definition.Schema for one collection: struct-derived
// JSON (UUIDv7 field ids) -> FromJSON -> rename to the spec collection name ->
// flag identity fields unique.
func deriveSchema(prefix string, spec collectionSpec) (*definition.Schema, error) {
	raw, err := shapeSchemaJSON(spec.suffix)
	if err != nil {
		return nil, fmt.Errorf("anansistore: schema for %s: %w", spec.suffix, err)
	}
	sch, err := definition.FromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("anansistore: parse schema for %s: %w", spec.suffix, err)
	}
	sch.Name = prefix + spec.suffix
	for _, name := range spec.unique {
		id, field := sch.FindField(name)
		if field == nil {
			return nil, fmt.Errorf("anansistore: schema %s has no field %q to mark unique", sch.Name, name)
		}
		field.Unique = true
		sch.Fields[id] = *field
	}
	return sch, nil
}

// shapeSchemaJSON dispatches data.SchemaFrom per concrete shape type (the
// extractor is generic and takes no value argument).
func shapeSchemaJSON(suffix string) ([]byte, error) {
	switch suffix {
	case "conversations":
		return data.SchemaFrom[conversationShape]()
	case "turns":
		return data.SchemaFrom[turnShape]()
	case "contents":
		return data.SchemaFrom[contentShape]()
	case "roles":
		return data.SchemaFrom[roleShape]()
	case "blobs":
		return data.SchemaFrom[blobShape]()
	case "blob_refs":
		return data.SchemaFrom[blobRefShape]()
	case "runs":
		return data.SchemaFrom[runShape]()
	case "receipts":
		return data.SchemaFrom[receiptShape]()
	case "settings":
		return data.SchemaFrom[settingsShape]()
	}
	return nil, fmt.Errorf("anansistore: no shape for collection %q", suffix)
}

// turnCompositeKey is the unique identity of one turn version: the domain ID
// and version joined by "@". anansi mints _id_; this field carries identity.
func turnCompositeKey(id string, version int) string { return fmt.Sprintf("%s@%d", id, version) }

// turnRefKey is the composite for a turn link (head/parent/forked-from);
// empty means no link.
func turnRefKey(id string, version int) string { return turnCompositeKey(id, version) }

// contentPageKey mirrors the memory store's page key exactly:
// key + "/" + zero-padded (20-digit) version, so lexicographic order equals
// (key asc, version asc) and cursors behave identical across adapters.
func contentPageKey(key string, version int) string { return key + "/" + pad20(version) }

func pad20(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
