## Writing TODOs

When starting on a new taks, write the steps to a file under the `todo` folder.

### Rules & Best Practices

* **Provide Enough Context:** Because tasks may be picked up by another agent or across different sessions, **never create single-phrase TODOs**. Each task must include enough background, intent, relevant file paths, links, or specific requirements so any agent can take over seamlessly without asking for context.
* **Track State Clearly:** Update task statuses promptly as work progresses.

### Task Status Identifiers

* `[ ]` Tasks that are planned.
* `[-]` Tasks that are in progress.
* `[*]` Tasks that are done.
* `[=]` Tasks that are skipped (include brief reasoning).
* `[X]` Tasks that are blocked (include the blocker details).

### Format Example

```markdown
- [ ] Implement user auth middleware
  - **Context:** Required for protecting `/api/v1/dashboard` routes.
  - **Details:** Use JWT validation based on existing spec in `docs/auth.md`.
  - **Files:** Modify `src/middleware/auth.js` and add unit tests to `tests/auth.test.js`.
```

## Anansi data modelling via structs

Anansi allows data modelling via structs because collection schemas can be derived from them — this makes systems actually type safe. Prefer defining a tagged struct over hand-writing schema JSON.

```go
type RunMetadata struct {
    WorkflowID string `anansi:"workflowId"`
}

type PipelineState struct {
    document.DocumentModel          // embeds system fields (_id_, _metadata_)
    Data    RunData      `anansi:"state"`
    RunInfo RunMetadata  `anansi:"run_metadata"` // NOT the same as reserved _metadata_
}

schemaBytes, err := data.ExtractDTOSchemaDirect(&PipelineState{})
// then json.Unmarshal into definition.Schema and p.CreateCollection(ctx, &sc)
```

Hard-won rules (learned the hard way against go-anansi v8.6.4):

* **Field IDs in schemas are UUIDv7s** (`Field ID 'x' is not a valid UUIDv7`). Hand-writing a schema requires minting UUIDv7 keys per field; deriving from structs handles this automatically.
* **Type mapping**: `map[string]any` → `record`; nested structs → `object` + nested schema; scalars map directly.
* **Reserved system containers**: `_id_` is a system-minted UUIDv7 primary key; `_metadata_` is reserved for SYSTEM metadata (request tracing, versioning). Never write application/run data into either. `Document.Set("_id_"/"_metadata_")` returns a readonly error — seed such fields in the record map BEFORE constructing the view.
* **Document factory must be configured** before any persistence op that mints documents (`data.ConfigureDocumentFactory`), otherwise panics with `ERR_DATA_FACTORY_NOT_CONFIGURED`. It is a singleton; configure once at startup.
* **Inserts preserve valid pre-set `_id_`s** — create documents in memory first, persist later (lazy insert) without losing identity.

