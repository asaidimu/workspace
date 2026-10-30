# Go AI Workspace Specification

Oct 15, 2026 · @Saidimu

The workspace is an embeddable Go library that keeps AI conversations as an immutable, branchable history of typed content. It brings its own command kernel, pluggable storage, LLM providers and agent loop, and runs standalone or inside hestia. This tab says what we are building and why, with the contracts needed to implement it in Go; persistence has its own tab, `Store specification.md` in this folder.

Implementing agents start with `Sources and agent brief.md` in this folder: pinned sources, a reading map and the build order.

Status: a complete draft for implementation. Section 17 lists the eleven decisions still open, each with a recommendation and the phase that settles it.

## 1. What we are building

We are building `workspace`, a Go library that gives any application durable, branchable AI conversations with tools, files and agents. It is a library first: an application calls `workspace.Open` and gets a complete engine, with no server required.

The library has seven parts:

- **Conversations** are trees of immutable turns, so users can edit, branch and fork without losing history.
- **Typed content** (text, documents, images, tool calls, preferences) passes through a registry that controls what the model sees and may produce.
- **Roles** bundle a persona, preferences and per-model constraints.
- **A prompt pipeline** turns conversation state into provider requests within a token budget.
- **Provider adapters** stream responses and call tools through one interface.
- **An agent loop** runs tools with limits, cancellation and crash-safe checkpoints.
- **A command kernel** makes every change atomic, authorized and observable, on top of a storage port with an anansi adapter.

It is not a chat UI, a model host, a vector database or a general workflow engine. It is the state and safety layer of an agent harness: durable history, atomic commands, validated blocks, scoped tools and events. A loop is policy on top of it, and section 11 shows how to write your own.

| Deliverable | What it is |
| --- | --- |
| Core module `workspace` | The library. Imports no hestia code. Module path assumed `github.com/asaidimu/workspace`. |
| Store implementations | `store/memory`, `store/anansistore` and the shared conformance suite. |
| Provider adapters | Anthropic, OpenAI, Gemini and OpenRouter (OpenAI-compatible), plus a replay provider for tests. |
| Host module `workspace-hestia` | A hestia module: command and event streams, an identity bridge and a resource authorizer. A separate Go module. |
| Reference CLI | A small program that exercises every scenario in section 4 end to end. |

## 2. Why

A Go rewrite lets the workspace ship inside hestia as one binary, and the other reasons are smaller than they first look. The TS package `@asaidimu/utils-workspace` 9.0.0 proved the model: a command kernel, a content registry, a versioned turn tree and a prompt pipeline. It runs in a JavaScript runtime, while hestia's desktop, CLI and HTTP surfaces are Go.

What Go genuinely buys us:

- One binary that shares types with hestia, with no JS runtime and no IPC boundary. This is the main reason. SQLite needs CGo, covered in section 14.
- No event-loop stalls: heavy JSON work, tokenizing or compaction cannot block streaming for other conversations, and runs are cheap goroutines.
- Cancellation through `context.Context` and streaming through iterators.
- A lean footprint and instant start-up for the desktop app, CLI and embedded targets.

What the TS version lacks. These are scope choices we make alongside the port, not benefits of Go, because TS could gain them too:

- No agent loop. A turn that needs tools returns a signal and stops.
- No streaming in the Google executor, and token accounting returns zeros.
- Retrieval, conflict resolution and compaction exist only as interfaces.
- Forking, session deletion and blob release have defects (appendix, section 18).

What Go does not buy us:

- **Raw speed.** LLM calls and database I/O dominate every turn, and Go's JSON handling is not reliably faster than V8's.
- **I/O concurrency.** `async` and `await` already run parallel tool calls and many streams well.
- **Ecosystem.** TypeScript has more LLM tooling, such as tokenizers, schema libraries and SDKs that track new API features first, so Go needs more hand-written code.

The case therefore rests on shipping one binary inside hestia. If that requirement relaxes, the alternative is to keep the stable TS library and run it as a sidecar process behind hestia's gateway, at the cost of a second runtime, an IPC boundary and packaging. Phase 0 measures the TS baseline, so every Go performance target is set against a real number.

We cannot simply host the workspace on hestia's kernel. hestia's dispatcher is a stateless router with a security chain: no transactions, no per-key ordering, no compensation, no event log. The workspace needs all four, so it carries its own kernel and hestia hosts it through an adapter.

| Option | Verdict | Reason |
| --- | --- | --- |
| Port the TS code one to one | Rejected | Carries over the defects and JS machinery such as deep-cloned state patches and a global promise lock. |
| Build it inside hestia only | Rejected | Needs state semantics hestia does not offer, couples the library to one host and is harder to test. |
| Standalone library plus hestia adapter | Chosen | Reusable in any Go program, testable without a host, and still a first-class hestia module. |

## 3. Goals, non-goals and success criteria

The workspace succeeds if a Go developer can embed it in an afternoon and trust it with real conversations. Eight goals follow.

1. **Embeddable.** Open the workspace, create a conversation and run a turn in about 30 lines of Go, error handling included.
2. **Faithful history.** Edits, branches and forks never mutate or lose a turn. A fork writes one row.
3. **Crash-safe runs.** Killing the process mid-run loses no committed turn, and the run resumes.
4. **Responsive.** Streaming events reach subscribers as the provider sends them, and cancel takes effect within a second.
5. **Provider-neutral.** Switch provider or model mid-conversation with no data loss.
6. **Storage-neutral.** Behaviour is defined by the store conformance suite, not by one database.
7. **Host-neutral.** The core module imports no hestia code.
8. **Secure by default.** Authorization is default-deny, data is isolated per owner, and agents run with narrower scopes than their users.

Non-goals for the first release:

- A user interface, model hosting or training.
- A vector database. Retrieval is a port with a simple default.
- Coordination between processes beyond the store's compare-and-swap.
- A durable event log or outbox. Events are in-process and best-effort; clients re-sync by querying.
- Encryption at rest beyond what the backend offers.
- Importing existing TS data. It may come later as a separate tool.

The targets below are hypotheses. Phase 0 measures the TS library on the same workload, and each Go target becomes "no slower than that baseline" unless the measurement says otherwise.

| Criterion | Target | How it is verified |
| --- | --- | --- |
| Fork a 1,000-turn conversation | One row written | Store conformance test |
| Load a 500-turn chain and assemble a prompt | Under 50 ms p95, excluding the provider, SQLite on a local SSD | Benchmark |
| Kill the process during an agent run | Run resumes, zero committed turns lost | Failure-injection test |
| Cancel a streaming turn | Provider request closed and run cancelled within 1 s | Integration test with the replay provider |
| Cross-owner access | None possible through any API | Conformance and end-to-end tests |
| Memory and anansi stores | Identical results over 10,000 random operations | Differential test |
| Dependency rule | No hestia import in the core, no anansi persistence import outside `store/anansistore` | CI import check |

## 4. Users and scenarios

Four kinds of people touch the workspace, and eight scenarios define what it must do for them.

| Persona | What they do | Where they meet the workspace |
| --- | --- | --- |
| Embedding developer | Opens a workspace in a Go app, registers tools and block types | Public API, section 13 |
| End user in a hestia app | Chats, edits, branches, attaches files, watches agents work | hestia messages and events, section 14 |
| Extension author | Adds tools, block types, providers or prompt sections | Registries and extension points, section 13 |
| Operator | Deploys, monitors and upgrades | Configuration, observability, migrations, section 15 |

| # | Scenario | Subsystems exercised |
| --- | --- | --- |
| S1 | Start a conversation with a role, send a message, stream the reply | Kernel, prompt pipeline, provider, store |
| S2 | Edit an earlier message, continue on a new branch, move between versions | Turn tree, kernel |
| S3 | Fork a conversation at any turn | Turn tree, store |
| S4 | Attach a document and an image and refer to them across turns | Blobs, content registry, provider mapping |
| S5 | Run an agent that calls tools in parallel and returns a result | Agent loop, tools |
| S6 | Cancel a running turn | Run manager, provider |
| S7 | Restart the process mid-run and resume | Run state, store, recovery |
| S8 | Switch provider or model in the middle of a conversation | Prompt pipeline, provider mapping |

## 5. System architecture

&#91;embedded content: architecture · 6 layers, 3 ports\]

The core calls storage, blobs and models only through ports, so replacing a backend, a vendor or a host changes exactly one shaded box.

The repository layout follows the drawing:

```
workspace/               facade: Open, Handle, Config
  domain/                pure types and rules
  kernel/                commands, units of work, authorizer, events
  content/               block registry and built-in types
  prompt/                pipeline, budget, retrieval
  provider/              interface and shared types; adapters in subpackages
  agent/                 run manager, loop, tools
  blob/                  blob port and adapters
  store/                 store port, memory and anansistore adapters, conformance suite
  workspacetest/         fakes and golden-test helpers
workspace-hestia/        separate Go module: the hestia host adapter
```

Five dependency rules hold everywhere, and CI fails any import that breaks them:

- `domain` imports only the standard library. Anansi reaches core in exactly two places: content may import its schema, validation and binding packages, and kernel may import `core/common` alone, for `SystemError` and `Issue`. No other core package imports anansi.
- Core packages import `domain` and the ports, never an adapter.
- Only `store/anansistore` imports anansi's persistence packages, only `workspace-hestia` imports hestia, and only the provider adapters talk to vendor APIs.
- Adapters are wired together in `workspace.Open` and nowhere else.
- The core module never imports the `workspace-hestia` module.

At runtime, commands run on the caller's goroutine under the keyed serializer. Each agent run is one goroutine owned by the run manager, and each provider stream has its own. Subscribers receive events on buffered channels.

## 6. Domain model

The domain is a small set of immutable records plus a few mutable pointers, and every rule in the workspace follows from eight invariants. Field-level definitions live in Store specification, section 5.

- **Conversation:** a title, a role, topics, attached content, preference keys, per-model constraints and a head pointer into the turn tree.
- **Turn:** an immutable message by a user, agent, tool or system actor, made of typed blocks.
- **Content:** keyed, versioned material (documents, preferences, artifacts) with an access mode.
- **Role:** a persona, preferences and per-model constraints.
- **Blob:** metadata for a file whose bytes live in the blob store.
- **Run:** the durable record of one agent execution, with a status, a step counter and checkpoint state.
- **Owner and principal:** every record belongs to one owner, and every action has a principal.

A turn is identified by (ID, version). An edit creates a new version of the same ID, so one position in the conversation can hold several alternatives. Each version points to a specific parent version.

```
t1.v1 ── t2.v1 ── t3.v1        conversation A: head = t3.v1
   └──── t2.v2 ── t4.v1        conversation B: head = t4.v1 (t2 was edited)
```

The invariants:

1. Turns are immutable. Nothing rewrites a turn after it is appended.
2. A turn's parent exists when the turn is appended.
3. A conversation's head is empty or points to an existing turn, and the active chain is the parent walk from the head.
4. All versions of one turn ID share a position: the same parent ID. The parent's version may differ if the parent was itself edited. We confirm this against TS fixtures in Phase 0.
5. Content versions are immutable, and readers see the latest non-tombstoned version.
6. Every record belongs to one owner, and no operation crosses owners.
7. Blocks reference blobs and content by ID, and the holder links are written in the same transaction as the turn.
8. Time and IDs come from an injected clock and a UUIDv7 generator.

| Operation | Effect | Writes |
| --- | --- | --- |
| Append | New child of the head, head moves to it | One turn, one head update |
| Edit | New version of an existing turn ID with the same parent, head moves to it | One turn, one head update |
| Switch version | Head moves to the newest leaf under the chosen version | One head update |
| Fork | New conversation whose head is the chosen turn | One conversation row |
| Delete conversation | Conversation removed, turns kept until swept | One row plus links |

Content has an access mode. These are the Go semantics, to be checked against the TS fixtures.

| Mode | Model sees it | Model can create or revise it | Typical use |
| --- | --- | --- | --- |
| read | Yes, as context | No | Reference documents, preferences |
| write | Yes, as an artifact | Yes, as new versions | Model-produced outputs |
| readwrite | Yes | Yes | Documents the model and user edit together |
| internal | No | No | System bookkeeping such as summaries |

Preferences are content of type `preference`. When they conflict, the conversation's beat the role's, and the role's beat the workspace settings. Conversation-level keys are stored on the conversation (`PreferenceKeys`), so a conversation can carry its own opinions without writing content the role also sees.

Constraints (temperature, maximum output tokens, stop sequences, thinking budget) follow the same precedence: the role's, then the conversation's, then the caller's per-run override. Every field the later source sets wins; a source that leaves a field unset contributes nothing, so the layers compose instead of replacing each other.

## 7. Kernel

The kernel turns every change into a command that runs authorized, ordered and atomic, and it publishes events only after the commit succeeds. It is generic and imports only the `store` interfaces, so it runs on any store.

A command passes through seven steps:

1. An envelope is built with the principal, IDs for tracing and an optional idempotency key.
2. The middleware chain runs, outermost first: panic recovery, tracing, logging, validation.
3. The authorizer decides, and the default is deny.
4. The keyed serializer admits the command in order for its conversation.
5. `store.Atomic` runs the handler, which reserves the idempotency key first when one is given.
6. The handler returns a result and events. Events are held until the commit.
7. After commit the events reach the bus. After rollback, compensation hooks run.

```go
package kernel

type Command interface {
	CommandName() string
}

type Event interface {
	EventName() string
}

type PrincipalKind int

const (
	UserPrincipal PrincipalKind = iota
	AgentPrincipal
	SystemPrincipal
)

type Principal struct {
	Owner   string
	Subject string
	Kind    PrincipalKind
	Scopes  []string
}

type Envelope struct {
	ID             string
	Name           string
	Principal      Principal
	At             time.Time
	CorrelationID  string
	CausationID    string
	IdempotencyKey string
	Command        Command
}

type Target struct {
	Kind string // "conversation", "content", "role", "run"; empty for owner-level commands
	ID   string
}

type Authorizer interface {
	Authorize(ctx context.Context, p Principal, commandName string, t Target) error
}

type Handler[C Command, R any] func(ctx context.Context, tx store.Tx, c C) (R, []Event, error)

type Next func(ctx context.Context, env Envelope) (any, error)

type Middleware func(next Next) Next

type EventEnvelope struct {
	Name           string
	Owner          string
	ConversationID string // empty for owner-level events
	CorrelationID  string
	At             time.Time
	Event          Event
}

type EventFilter struct {
	Owner          string
	ConversationID string   // empty matches all
	Names          []string // empty matches all
}

type Bus interface {
	Subscribe(ctx context.Context, f EventFilter, buffer int) (<-chan EventEnvelope, error)
}

func New(s store.Store, opts ...Option) (*Kernel, error)

func Register[C Command, R any](k *Kernel, h Handler[C, R]) error

func Dispatch[C Command, R any](ctx context.Context, k *Kernel, p Principal, c C, opts ...DispatchOption) (R, error)
```

The constructors, options and the clock and ID interfaces that these types rely on:

```go
package kernel

type Kernel struct {
	// unexported fields
}

type config struct {
	// unexported fields
}

type dispatchConfig struct {
	// unexported fields
}

type Option func(*config)

type DispatchOption func(*dispatchConfig)

func WithMiddleware(m ...Middleware) Option

func WithAuthorizer(a Authorizer) Option

func WithClock(c domain.Clock) Option

func WithIDs(g domain.IDGenerator) Option

func AllowOwner() Authorizer

func WithIdempotencyKey(key string) DispatchOption

func WithCorrelation(id string) DispatchOption

func (k *Kernel) Bus() Bus
```

```go
package domain

type Clock interface {
	Now() time.Time
}

type IDGenerator interface {
	NewID() string // a UUIDv7 string
}
```

Rules that the types alone do not say:

- **Authorization.** Scopes are strings like `workspace:turn:append`. The authorizer checks scopes and resource rules, for example that agents cannot delete conversations. The store's owner scoping is a second, independent wall. Opening a workspace without an authorizer is an error, and `AllowOwner()` is provided for single-user apps.
- **Ordering.** The serializer key is the conversation ID, or the owner ID for owner-level commands. It is FIFO per key, waiting honours the context, and it is never held across a provider call. Correctness across processes comes from the store's compare-and-swap, not from this lock.
- **Idempotency.** A repeated key returns the stored result. Results of idempotent commands must therefore be JSON-serializable.
- **Events.** Delivery is in-process, ordered per conversation and at most once. A subscriber that cannot keep up is disconnected after a final overflow event and must re-sync by querying.
- **Errors.** Every error the kernel returns is anansi's `*common.SystemError` (`core/common`), not a bespoke error type: it is more detailed than anything the kernel could define, and it is already what content and the anansi-backed layers emit, so one type spans the whole stack. `SystemError` carries a machine-readable `Code`, a human `Message`, the `Operation` that failed (the command name), the `Path` it failed on (the target ID), a `Severity`, a `Retryable` flag, structured `Issue`s for per-field detail, and a `Cause`; `Unwrap` returns the cause, so `errors.Is` and `errors.As` reach through every layer. The kernel's error classes are the codes `NOT_FOUND`, `CONFLICT`, `INVALID`, `FORBIDDEN`, `BUSY`, `CANCELED`, `UNAVAILABLE`, `INTERNAL`. Store sentinels map as follows: not-found to `NOT_FOUND`, conflict and already-exists to `CONFLICT`, invalid and too-large to `INVALID`, busy to `BUSY`, everything else to `INTERNAL`. The store sentinel stays on the cause chain, so `errors.Is(err, store.ErrNotFound)` keeps working alongside a code check. Callers branch on `Code`, or use `errors.As` for the full structure; `Retryable` is set whenever repeating the identical call is safe.

| Command | Effect | Event |
| --- | --- | --- |
| `settings.update` | Change workspace settings | `settings.updated` |
| `conversation.create` | Create a conversation | `conversation.created` |
| `conversation.update` | Title, role, topics, attachments, status | `conversation.updated` |
| `conversation.fork` | New conversation at a chosen turn | `conversation.created` |
| `conversation.delete` | Remove a conversation | `conversation.deleted` |
| `turn.append` | Add a user or system turn | `turn.appended` |
| `turn.edit` | New version of an existing turn | `turn.appended` |
| `turn.switch` | Move the head to another version | `conversation.head_moved` |
| `run.start` | Create a run and start the agent loop | `run.started` |
| `run.cancel` | Cancel a run | `run.cancelled` |
| `run.approve` | Approve or reject a pending tool call | `run.resumed` |
| `content.put`, `content.tombstone` | Write a content version or hide a key | `content.changed` |
| `role.put`, `role.delete` | Create, change or remove a role | `role.changed` |
| `blob.register` | Register metadata after the bytes are stored | `blob.registered` |

## 8. Content registry

The registry decides what each block type means: how it validates, how the model sees it, which provider parts it maps to, and what happens after it is committed. Turns and content both hold blocks, so one registry serves both.

A block type declares two capabilities. **Consumable** means blocks of that type may appear in prompts. **Emittable** means the model may produce them. Content access modes from section 6 map onto these: read is consumable, write is emittable and shown back as an artifact, readwrite is both, and internal is neither.

```go
package content

// definition and common are anansi's schema and error packages.

type Capability uint8

const (
	Consumable Capability = 1 << iota
	Emittable
)

type BlockType interface {
	Name() string
	Capabilities() Capability
	Schema() *definition.Schema          // the payload DTO as an anansi schema
	Predicates() definition.PredicateMap // cross-field and semantic rules, may be nil
	Describe() string
	Rules() []string
	Decode(payload map[string]any) (any, []common.Issue) // validates strictly, then binds to the typed value
	Encode(value any) (map[string]any, error)
	Render(value any) (string, error)
}

type typeConfig struct {
	// unexported fields
}

type TypeOption func(*typeConfig)

func WithPredicates(p definition.PredicateMap) TypeOption

func WithDescription(text string) TypeOption

func WithRender(fn func(value any) (string, error)) TypeOption

func WithRules(rules []string) TypeOption

// NewBlockType derives the schema from T's anansi tags, compiles the validator once,
// and returns a BlockType whose typed value is a T.
func NewBlockType[T any](name string, caps Capability, opts ...TypeOption) (BlockType, error)

type Effects interface {
	Effects(ctx context.Context, b domain.Block) ([]kernel.Command, error)
}

// MapEnv is what a mapper needs beyond the block: the blob resolver and the
// owner the read is scoped to. An empty environment resolves nothing.
type MapEnv struct {
	Blobs BlobResolver
	Owner string
}

// BlobResolver turns a block's blob reference into a provider part, either a
// media Source the adapter streams from or a provider-side remote id.
type BlobResolver interface {
	Resolve(ctx context.Context, owner, ref, providerName string) (provider.Part, error)
}

type Mapper interface {
	ToParts(ctx context.Context, b domain.Block, c provider.Capabilities, env MapEnv) ([]provider.Part, error)
}

type Diagnostic struct {
	BlockIndex int
	Code       string // "unknown_type", "invalid", "downgraded"
	Issues     []common.Issue
}

type registration struct {
	mappers map[string]Mapper
	effects Effects
}

type RegisterOption func(*registration)

func WithMapper(providerName string, m Mapper) RegisterOption

func WithEffects(e Effects) RegisterOption

type Registry interface {
	Register(t BlockType, opts ...RegisterOption) error
	Lookup(name string) (BlockType, bool)
	Types() []BlockType
	MapperFor(typeName, providerName string) (Mapper, bool)
	EffectsFor(typeName string) (Effects, bool)
	Parse(raw json.RawMessage) ([]domain.Block, []Diagnostic, error)
	ParseText(text string) ([]domain.Block, []Diagnostic, error)
}
```

Built-in types:

| Name | Consumable | Emittable | Notes |
| --- | --- | --- | --- |
| `text` | Yes | Yes | Plain or markdown text |
| `thinking` | No | Yes | Stored for display, not sent back unless a provider requires it |
| `image` | Yes | No | Blob reference with media type |
| `document` | Yes | Yes | Blob or inline text, with a title |
| `file` | Yes | No | Any other blob reference |
| `json` | Yes | Yes | Structured data validated by a schema |
| `preference` | Yes | No | A named instruction, folded into the prompt's preferences section |
| `tool_call` | No | Yes | Tool name, call ID and arguments |
| `tool_result` | Yes | No | Call ID, output and an error flag |
| `unknown` | No | No | Anything the registry cannot interpret, kept verbatim |

Rules:

- **Unknown blocks survive.** They are kept byte-for-byte in turns, reported as diagnostics, never sent to a provider without a mapper, and never lost by edits or forks.
- **Invalid model output is not dropped.** A block that fails validation is stored as `unknown` with an `invalid` diagnostic. The agent loop may ask the model to repair it once.
- **Naming.** Built-in names are reserved. Extensions use a namespaced name such as `acme.chart`. A duplicate registration is an error.
- **Provider mapping.** Each built-in has a default mapper per provider. When a provider lacks a capability, the block is downgraded, for example an image becomes a text placeholder, with a `downgraded` diagnostic. It never fails silently.
- **Effects.** After a turn commits, each block's effects are dispatched as commands under the turn's principal. The idempotency key is the turn reference plus the block index, so a replay changes nothing.
- **Structured-output mode.** JSON-only output is the opt-in fallback, not the default. The prompt then opens with the TS operating-system preamble and a `blocks` section describing the block types and the JSON envelope. The registry itself does not carry the output schema; the caller supplies it per run.

### Hybrid output: prose plus workspace fences

The default output mode is hybrid. The model replies in ordinary prose and, when it needs typed content, embeds a fenced block:

````
```workspace:<type>
{...one JSON object for the block value...}
```
````

The grammar is deliberately small:

- An opening fence is a line whose content is ````` ```workspace `` optionally followed by `:<type>`, at most one space after the backticks. The closing fence is a line of three backticks.
- The fence body is exactly one JSON object: the block value, with the same fields the type's schema defines. The TS `{type, value}` envelope is accepted too.
- Everything outside fences is prose and becomes a `text` block, in order.
- A fence with an unknown or non-emittable type, an unclosed fence, or a body that is not one object is kept as an `unknown` block with a diagnostic. Nothing is dropped.
- `tool_call` fences are accepted only when the provider has no native tool channel; native calls always win.

Mapping onto the six checks: parsing is check 1 (the fence is located and its body decoded once), resolution is check 2 (the type lookup), validation and meaning are checks 3 and 4 (the type's schema and predicates run exactly as they do for tool payloads), repair is check 5 (a diagnostic, with the raw fence preserved as `unknown`), and binding is check 6.

### Validating produced blocks

anansi's validator enforces that produced blocks are correct. A block from the model, a user or a tool is committed as a typed block only if it passes six checks:

1. **Parse.** The adapter completes the part, with tool arguments complete only at `PartDone`, and the payload is decoded from JSON once, into a map.
2. **Resolve.** The registry looks the type up by name. An unknown type is kept as `unknown` with a diagnostic and is never committed as typed.
3. **Validate.** The type's compiled `DocumentValidator` checks the payload in strict mode: required fields, types, enums, ranges, formats, nested objects and arrays, and unexpected fields. It returns structured issues, each with a code, a path and a severity.
4. **Check meaning.** Predicates registered with the type enforce what a schema cannot: referenced blobs and content exist and belong to the owner, written content keys are writable, and sizes are within caps.
5. **Repair or downgrade.** The issues go back to the model once, as an error `tool_result` for a tool call or a repair message otherwise. If the second attempt fails, the block is stored as `unknown` with an `invalid` diagnostic. An invalid block is never stored as typed.
6. **Bind and store.** A valid payload is bound into its typed Go value with anansi's cached binder, and the store's own schema validates the stored envelope on write.

Two layers therefore enforce correctness: the registry validates the payload, which is free-form at the storage level, and the store validates the envelope. Tool-call arguments get a third check before a tool runs: they are validated against the tool's JSON Schema, the same one sent to the provider, by the content package's validator. That validator implements the subset the workspace's converter emits (`type`, `enum`, `required`, `properties`, `additionalProperties` and `items`), so a schema that came through `content.JSONSchema` is checked faithfully. A call that fails becomes an error `tool_result` and the tool does not run.

What anansi provides for block types and DTOs:

| Need | anansi mechanism |
| --- | --- |
| Define a DTO once | A Go struct with `anansi` tags. The schema is derived from it, and codegen can generate the struct from a schema instead. |
| Validate | `DocumentValidator` in strict, partial or loose mode, with predicates and structured issues |
| Bind | A cached binder between documents or maps and structs, with coercion |
| Narrow shapes | Projections, and `ReadAs`, `CreateFrom` and `UpdateFrom` on collections |
| Redact outputs | `sanitize` field masks |
| Client types | Codegen to TypeScript from the same schema |
| Test data | A faker generator in `codegen/faker`, whose output needs checking |
| JSON Schema for providers | Not provided. anansi's docs say a translator would have to be written, so tool schemas are authored or derived as JSON Schema and validated with anansi's JSON Schema compiler. |

## 9. Prompt pipeline

The pipeline turns a conversation snapshot into a provider request in six deterministic stages, so the same inputs always give a byte-identical prompt. That property makes golden tests possible and makes provider switching safe.

1. **Snapshot.** Read the conversation, role, settings, attached content and the active chain in one read transaction.
2. **Select.** Gather candidate content: attachments, role and workspace preferences, and items from the retriever.
3. **Resolve.** Pick versions, drop tombstoned keys, apply preference precedence and record conflicts as diagnostics.
4. **Compact.** Fit the token budget, as described below.
5. **Assemble.** Build the ordered system sections and the transcript messages.
6. **Map.** Convert blocks to provider parts, apply capability downgrades, and attach tool specs and an output schema when needed.

```go
package prompt

type Stage interface {
	Name() string
	Run(ctx context.Context, b *Build) error
}

type Build struct {
	Snapshot    Snapshot
	Candidates  []Candidate
	Sections    []Section
	Transcript  []domain.Turn
	Budget      Budget
	Request     provider.Request
	Diagnostics []Diagnostic
}

type Section struct {
	ID    string
	Title string
	Body  string
	Order int
}

type Budget struct {
	ContextWindow  int
	ReservedOutput int
	Used           int
}

type RetrievalQuery struct {
	Owner          string
	ConversationID string
	Topics         []string
	Text           string
	Limit          int
}

type Scored struct {
	Ref   store.ContentRef
	Score float64
}

type Retriever interface {
	Retrieve(ctx context.Context, q RetrievalQuery) ([]Scored, error)
}

type Compactor interface {
	Compact(ctx context.Context, turns []domain.Turn, targetTokens int) (Summary, error)
}

type Tokenizer interface {
	Count(model string, parts []provider.Part) (int, error)
}

type Pipeline struct {
	stages []Stage
}

func NewPipeline(stages ...Stage) *Pipeline

func (p *Pipeline) Build(ctx context.Context, in Input) (provider.Request, []Diagnostic, error)
```

`Snapshot`, `Candidate`, `Summary`, `Input` and `Diagnostic` are plain structs holding the data named in the stages above.

System sections appear in this order:

| Order | Section | Content | Present when |
| --- | --- | --- | --- |
| 1 | policy | Fixed operating rules from the workspace | Always |
| 2 | tools | Tool descriptions | Structured-output mode only; native and hybrid modes pass tools in the request |
| 3 | blocks | Which block types the model may emit | Structured-output mode, or hybrid mode when a registry is wired |
| 4 | persona | The role's persona | A role is set |
| 5 | preferences | Merged preferences | Any exist |
| 6 | context | Content in read mode | Any exists |
| 7 | artifacts | Latest versions of write and readwrite content | Any exists |
| 8 | instructions | Per-run instructions | Supplied by the caller |

Extensions add sections with an anchor: `prepend`, `append`, `before:<section>` or `after:<section>`. A position that names a section that was not assembled degrades to append with a diagnostic.

Sections render as `# Title` followed by the body and are joined into one system text part with `---` separators, matching the TS wire format. Sections whose body carries its own headers (the structured preamble, the tools and blocks bodies) are emitted verbatim without a title.

In hybrid mode the `blocks` section is generated from the registry: the fence grammar, the emittable types and their descriptions, and the type rules. In structured mode it describes the JSON envelope instead and forbids prose.

The budget works as follows:

- **Window.** Budget is the model's context window minus reserved output. Reserved output is the request's maximum output tokens, or 10% of the window if unset.
- **Fixed sections first.** If they alone exceed the budget, the build fails with a budget error, because that is a configuration problem.
- **Transcript newest first.** Turns are added until the budget is used. A tool call and its result are kept or dropped together.
- **Compaction.** When turns are elided and a compactor is configured, a stored summary replaces them. Summaries are internal content created in the background, never inside the user's turn, and the next build picks them up.
- **Counting.** A tokenizer per model counts tokens. The default estimator is bytes divided by four, rounded up, and is marked approximate in diagnostics.

The default retriever ranks candidates by topic overlap with the conversation, then by recency, with a configurable limit (default 20). Full-text or embedding retrievers plug in behind the same interface. The TS library defined this interface but never implemented it.

The stored transcript is provider-neutral, so switching providers (scenario S8) only changes stage 6. Thinking blocks are dropped on a switch, and remote file IDs are re-uploaded when they are missing or expired. Assembly is deterministic: sections sort by order then ID, maps are read in key order, and nothing uses the clock or randomness.

## 10. Providers

Every model sits behind one `Provider` interface that streams typed events, calls tools natively, reports usage and never leaks vendor types into the core. The prompt pipeline produces a `Request`, the adapter talks to the vendor, and the agent loop consumes the `Stream`.

```go
package provider

type Capabilities struct {
	Streaming        bool
	NativeTools      bool
	StructuredOutput bool
	Vision           bool
	PDF              bool
	Thinking         bool
	FileUpload       bool
	PromptCaching    bool
	ContextWindow    int
	MaxOutputTokens  int
}

type Source interface {
	Open(ctx context.Context) (io.ReadCloser, error)
	Size() int64
	MediaType() string
}

type Part interface {
	part()
}

type TextPart struct{ Text string }

type ImagePart struct {
	Source   Source
	RemoteID string // set when the file was uploaded to this provider
}

type FilePart struct {
	Name     string
	Source   Source
	RemoteID string
}

type ThinkingPart struct {
	Text      string
	Signature string
}

type ToolCallPart struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
}

type ToolResultPart struct {
	CallID  string
	Output  json.RawMessage
	IsError bool
}

type Role int

const (
	UserRole Role = iota
	AssistantRole
	ToolRole
)

type Message struct {
	Role  Role
	Parts []Part
}

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Request struct {
	Model           string
	System          []Part
	Messages        []Message
	Tools           []ToolSpec
	OutputSchema    json.RawMessage
	MaxOutputTokens int
	Temperature     *float64
	Stop            []string
	Metadata        map[string]string
}

type Usage struct {
	InputTokens     int
	OutputTokens    int
	CachedTokens    int
	ReasoningTokens int
	Estimated       bool
}

type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishError         FinishReason = "error"
)

type Response struct {
	Message           Message
	Usage             Usage
	Finish            FinishReason
	Model             string
	ProviderRequestID string
}

type EventKind int

const (
	PartStarted EventKind = iota
	PartDelta
	PartDone
	UsageReported
	Finished
)

type Event struct {
	Kind   EventKind
	Index  int    // part index within the message
	Delta  string // text or argument fragment for PartDelta
	Part   Part   // the complete part for PartDone
	Usage  *Usage
	Finish FinishReason
}

type Stream interface {
	Recv() (Event, error) // io.EOF after Finished
	Close() error
}

type Pricing struct {
	Currency            string // the currency the vendor bills in, usually USD
	InputMicrosPerMTok  int64
	OutputMicrosPerMTok int64
	CachedMicrosPerMTok int64
}

type ModelInfo struct {
	ID           string
	Capabilities Capabilities
	Pricing      Pricing
}

type RemoteFile struct {
	ID        string
	ExpiresAt time.Time // zero when it does not expire
}

type Provider interface {
	Name() string
	Models(ctx context.Context) ([]ModelInfo, error)
	Capabilities(model string) (Capabilities, error)
	Generate(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request) (Stream, error)
	UploadFile(ctx context.Context, src Source, name string) (RemoteFile, error)
}

type CredentialSource interface {
	Credential(ctx context.Context, providerName string) (string, error)
}
```

OpenRouter needs no separate wire implementation: `provider/openrouter` reuses
the OpenAI-compatible adapter with the gateway's base URL, provider name and
attribution headers, and reads its catalogue from `GET /models` (context
window, input modalities, supported parameters and per-token prices). Model IDs
the catalogue does not know still run with permissive capabilities, because the
gateway adds models without notice; `UploadFile` returns unsupported since
OpenRouter has no Files API.

Behaviour every adapter must follow:

- **Streaming.** Events arrive in order, `Recv` returns `io.EOF` after `Finished`, and `Close` is safe at any time. Cancelling the context closes the HTTP body.
- **Tool calls.** A `ToolCallPart` is complete only at `PartDone`. Argument fragments are exposed as deltas for UIs and are never executed.
- **Retries.** Only before the first response byte: connection errors, 429 and 5xx, with exponential backoff and full jitter (base 500 ms, cap 20 s, at most 4 attempts) that honours `Retry-After`. After a stream has begun there is no retry inside the adapter. The error carries the partial parts and the agent loop decides.
- **Timeouts.** Connect 10 s, first byte 60 s, idle between chunks 60 s, all configurable.
- **Unsupported features.** `UploadFile` returns an unsupported error when `FileUpload` is false. Requests using a capability the model lacks fail fast with a clear error, since downgrading is the prompt pipeline's job.
- **Credentials.** Keys come from a `CredentialSource` (environment, static value or the host's secret store). They are never stored in the store and never logged.
- **Dependencies.** Adapters use plain `net/http` and one shared SSE decoder. Vendor SDKs are not required.
- **Test providers.** A `replay` provider serves recorded transcripts and a `scripted` provider returns fixed events, so no test needs a network.

Cost is computed in integers. Adapters declare list prices in the vendor's currency, usually USD. The workspace converts them with a configured rate, expressed as micro-KES per unit of vendor currency, and stores `Usage.CostMicros` in KES on the assistant turn. When a vendor reports no usage, the tokenizer's estimate is stored and flagged as estimated.

## 11. Agent loop and tools

&#91;embedded content: agent loop · 5 steps, 2 decisions, crash recovery\]

A restart repeats at most the step in flight: one model call, or the tool calls that had no committed result. The assistant turn, tool calls included, is committed before any tool runs, and results are committed before the next step.

The agent package defines tools and the run manager:

```go
package agent

type Risk int

const (
	RiskLow Risk = iota
	RiskConfirm // pauses the run until a person approves
)

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Scopes      []string      // scopes the calling principal must hold
	Risk        Risk
	Idempotent  bool
	Timeout     time.Duration // zero selects the run default
	MaxOutput   int           // bytes; larger results are truncated and flagged
}

type ToolCall struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
}

type ToolResult struct {
	Output  json.RawMessage
	IsError bool
}

type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, p kernel.Principal, call ToolCall) (ToolResult, error)
}

func NewTool[In, Out any](spec ToolSpec, fn func(ctx context.Context, p kernel.Principal, in In) (Out, error)) Tool

type Registry interface {
	Register(t Tool) error
	Lookup(name string) (Tool, bool)
	Specs(p kernel.Principal) []provider.ToolSpec // only tools the principal may use
}

type RunOptions struct {
	Model         string
	MaxSteps      int
	MaxParallel   int
	Deadline      time.Duration
	CostCapMicros int64
	Instructions  string
}

type Manager interface {
	Start(ctx context.Context, p kernel.Principal, conversationID string, opts RunOptions) (domain.Run, error)
	Cancel(ctx context.Context, p kernel.Principal, runID string) error
	Approve(ctx context.Context, p kernel.Principal, runID, callID string, approve bool, reason string) error
	Recover(ctx context.Context) error // claims stale runs and resumes them
}
```

`NewTool` derives the JSON Schema from the input type's struct tags, so a tool is a typed function plus a spec. The schema library is chosen in Phase 0.

Rules for the loop:

- **States.** A run is queued, running, waiting for approval, succeeded, failed or cancelled. It only moves forward, except between running and waiting.
- **Commit order.** Each commit is one `Atomic` call that appends the turn and checkpoints the run with a compare-and-swap on the step counter.
- **Tools run at least once.** Calls without a committed result are re-run after a restart, with the call ID as the idempotency key. A tool that is not marked idempotent and may already have run is not re-run automatically. The run pauses for approval, flagged as possibly executed.
- **Parallelism.** Up to four calls per step by default, with results ordered by call index.
- **Narrowed identity.** Tools run under an agent principal whose scopes are the intersection of the user's scopes and the role's allowed tools. No tool can widen them.
- **Approval.** A `RiskConfirm` tool pauses the run and emits `run.approval_requested`. Approving resumes it. Rejecting or letting the 24-hour window lapse becomes an error result, so the model can adapt.
- **Limits.** Steps (25), deadline (10 minutes) and cost cap in micro-KES. Exceeding any one fails the run with a specific code. All committed work stays in the history.
- **Cancellation.** `run.cancel` cancels a context the run manager owns, never the caller's request. The provider stream closes, running tools see a cancelled context, and uncommitted partial text is discarded.
- **Repair.** An invalid block from the model gets one repair attempt per step.
- **Recovery.** A heartbeat every 10 seconds marks a run alive. `Recover` runs at start-up and periodically, and the store's claim operation guarantees one claimant per run.
- **Streaming.** Text deltas reach subscribers as `run.delta` events and are never stored.

### Building your own loop

The built-in loop is a small policy over a public step primitive. A step is one model call plus the execution of the tool calls it asks for, committed in the order described above. A custom loop calls `Step` itself and decides when to stop, when to compact, which tools to expose, and when to branch or fork. It still gets durable history, authorization, validation, events and cost accounting, because those belong to the step and not to the loop. A loop that wants crash recovery keeps its own state in the run's checkpoint.

```go
package agent

type StepOptions struct {
	Model        string
	MaxParallel  int
	Instructions string
	Tools        []string // tool names to expose; empty means every tool the principal may use
}

type StepResult struct {
	Turns      []domain.Turn         // the assistant turn and, when tools ran, the tool-result turn
	Finish     provider.FinishReason
	Pending    []ToolCall            // calls waiting for approval
	Usage      provider.Usage
	CostMicros int64                 // in micro-KES
}

type Stepper interface {
	Step(ctx context.Context, p kernel.Principal, conversationID string, opts StepOptions) (StepResult, error)
}

type Checkpointer interface {
	Checkpoint(ctx context.Context, p kernel.Principal, runID string, expectedStep int, state json.RawMessage) (domain.Run, error)
}
```

The manager implements both interfaces. This is a complete custom loop, which stops when the model finishes without asking for tools:

```go
func research(ctx context.Context, s agent.Stepper, p kernel.Principal, conversationID string) error {
	for i := 0; i < 8; i++ {
		res, err := s.Step(ctx, p, conversationID, agent.StepOptions{Model: "anthropic/claude-sonnet-5-5"})
		if err != nil {
			return err
		}
		if res.Finish != provider.FinishToolCalls && len(res.Pending) == 0 {
			return nil
		}
	}
	return errors.New("step budget exhausted")
}
```

The first release does not include sub-agents, MCP tool servers or sandboxed code execution. They would be tools or loops built on these same primitives.

## 12. Storage and blobs

Persistence is a port: the kernel and domain see only the `store` interfaces, and Store specification defines them in full. Metadata lives in the store, and file bytes live in a separate blob store.

The kernel uses the store in three ways. Each command runs inside one `Atomic` call. The prompt pipeline reads the active chain through `Chain`. Maintenance sweeps remove unreachable turns, orphaned blob metadata and expired receipts. Two implementations exist from day one, `store/memory` and `store/anansistore`, and both must pass the same conformance suite, which also runs on anansi's in-memory ephemeral backend.

Blob bytes sit behind a small port:

```go
package blob

type Info struct {
	Checksum  string // "sha256:<hex>"
	Size      int64
	MediaType string
}

type PutOptions struct {
	MediaType string
}

type Store interface {
	Put(ctx context.Context, r io.Reader, opts PutOptions) (Info, error)
	Open(ctx context.Context, checksum string) (io.ReadSeekCloser, error)
	Stat(ctx context.Context, checksum string) (Info, error)
	Delete(ctx context.Context, checksum string) error
	List(ctx context.Context, after string, limit int) ([]Info, error)
}
```

Implementations are an adapter over `asaidimu/blobs`, a plain filesystem store for tests and small deployments, and an in-memory store. The adapter's readiness for v1.3.x is a Phase 0 check.

A file enters the system in three steps, always in this order:

1. Stream the bytes to `Put` with no transaction open. The checksum is computed while streaming.
2. In the same transaction that appends the turn, the `blob.register` command registers the metadata and the holder link. It verifies the bytes exist with `Stat`.
3. If a provider needs its own copy, upload it outside any transaction, then record the remote ID with a short transaction.

Bytes are written before metadata, so metadata can never point at missing bytes. A crash between steps 1 and 2 leaves orphan bytes. A reconcile job removes bytes whose checksum has no metadata under any owner after a grace period. Bytes are deleted only when no owner's metadata references the checksum, and that check runs in system scope.

Maintenance runs in a background goroutine every hour by default. A host with its own scheduler can disable that and call the admin sweep instead.

## 13. Public Go API and extension points

Applications use a small facade: `Open` returns a `Workspace`, and `For(principal)` returns a handle whose methods are scoped to that principal's owner. Everything else is an extension point.

```go
package workspace

type Config struct {
	Store       store.Store
	Blobs       blob.Store
	Providers   []provider.Provider
	Credentials provider.CredentialSource
	Authorizer  kernel.Authorizer
	Registry    content.Registry
	Pipeline    *prompt.Pipeline
	Tools       []agent.Tool
	Clock       Clock
	IDs         IDGenerator
	Logger      *slog.Logger
	Options     Options
}

type Workspace struct {
	// unexported fields
}

func Open(ctx context.Context, cfg Config) (*Workspace, error)

func (w *Workspace) For(p kernel.Principal) *Handle

func (w *Workspace) Events() kernel.Bus

func (w *Workspace) Admin() Admin

func (w *Workspace) Close(ctx context.Context) error

type Handle struct {
	// unexported fields
}

func (h *Handle) Conversations() Conversations
func (h *Handle) Turns() Turns
func (h *Handle) Runs() Runs
func (h *Handle) Content() Content
func (h *Handle) Roles() Roles
func (h *Handle) Blobs() Blobs

type Conversations interface {
	Create(ctx context.Context, in CreateConversation) (domain.Conversation, error)
	Get(ctx context.Context, id string) (domain.Conversation, error)
	List(ctx context.Context, f store.ConversationFilter, p store.PageRequest) (store.Page[domain.Conversation], error)
	Update(ctx context.Context, id string, in UpdateConversation) (domain.Conversation, error)
	Fork(ctx context.Context, id string, at store.TurnRef, title string) (domain.Conversation, error)
	Delete(ctx context.Context, id string) error
}

type Turns interface {
	Append(ctx context.Context, conversationID string, in AppendTurn) (domain.Turn, error)
	Edit(ctx context.Context, conversationID string, ref store.TurnRef, in EditTurn) (domain.Turn, error)
	Switch(ctx context.Context, conversationID string, ref store.TurnRef) (domain.Conversation, error)
	Chain(ctx context.Context, conversationID string, opts store.ChainOptions) ([]domain.Turn, error)
	Branches(ctx context.Context, conversationID string, ref store.TurnRef) (BranchInfo, error)
}

type Runs interface {
	Start(ctx context.Context, conversationID string, in StartRun) (domain.Run, error)
	Get(ctx context.Context, runID string) (domain.Run, error)
	Cancel(ctx context.Context, runID string) error
	Approve(ctx context.Context, runID string, callID string, approve bool, reason string) error
}

type Content interface {
	Put(ctx context.Context, c domain.Content, baseVersion int) (domain.Content, error)
	Get(ctx context.Context, ref store.ContentRef) (domain.Content, error)
	List(ctx context.Context, f store.ContentFilter, p store.PageRequest) (store.Page[domain.Content], error)
	Tombstone(ctx context.Context, key string, baseVersion int) error
}

type Roles interface {
	Put(ctx context.Context, r domain.Role, expectedRev int64) (domain.Role, error)
	Get(ctx context.Context, id string) (domain.Role, error)
	List(ctx context.Context, f store.RoleFilter, p store.PageRequest) (store.Page[domain.Role], error)
	Delete(ctx context.Context, id string, expectedRev int64) error
}

type Blobs interface {
	Upload(ctx context.Context, r io.Reader, opts UploadOptions) (domain.Blob, error)
	Open(ctx context.Context, id string) (io.ReadSeekCloser, error)
}

type Admin interface {
	Sweep(ctx context.Context) (SweepSummary, error)
	Migrate(ctx context.Context) error
	Health(ctx context.Context) (Health, error)
}
```

Input structs such as `CreateConversation`, `AppendTurn` and `StartRun` carry the fields of the matching command in section 7. A run reports progress through the event bus, not through a return channel, so one subscription serves the whole conversation.

The package also defines `Clock` and `IDGenerator` (aliases of the `domain` interfaces), `Options` (the settings in section 15), `LocalUser(owner)` (a principal holding every scope), `Text(...)` (builds a text block), `BranchInfo` (the sibling versions of a turn and which one is active), `UploadOptions`, `SweepSummary` and `Health`. `Handle.Runs()` delegates to the agent manager from section 11.

This example meets the embedding goal from section 3:

```go
func run(ctx context.Context, st store.Store, bs blob.Store, prov provider.Provider) error {
	ws, err := workspace.Open(ctx, workspace.Config{
		Store:      st,
		Blobs:      bs,
		Providers:  []provider.Provider{prov},
		Authorizer: kernel.AllowOwner(),
	})
	if err != nil {
		return err
	}
	defer ws.Close(ctx)

	h := ws.For(workspace.LocalUser("me"))
	conv, err := h.Conversations().Create(ctx, workspace.CreateConversation{Title: "Hello"})
	if err != nil {
		return err
	}
	if _, err := h.Turns().Append(ctx, conv.ID, workspace.AppendTurn{Blocks: workspace.Text("Hi")}); err != nil {
		return err
	}
	events, err := ws.Events().Subscribe(ctx, kernel.EventFilter{Owner: "me", ConversationID: conv.ID}, 64)
	if err != nil {
		return err
	}
	if _, err := h.Runs().Start(ctx, conv.ID, workspace.StartRun{Model: "anthropic/claude-sonnet-5-5"}); err != nil {
		return err
	}
	for ev := range events {
		fmt.Println(ev.Name)
	}
	return nil
}
```

The subscription is opened before the run starts, so no early event is missed. The loop ends when the context is cancelled.

| Extension point | How it plugs in | Example |
| --- | --- | --- |
| Block types | `Registry.Register` | A chart block with its own renderer |
| Tools | `Config.Tools` | Calculator, file search |
| Providers | `Config.Providers` | A local model server |
| Command middleware | `kernel.WithMiddleware` | Audit log, rate limiting |
| Prompt sections and stages | `prompt.NewPipeline` with section anchors | A company policy section |
| Retriever and compactor | Pipeline options | Full-text retrieval |
| Authorizer | `Config.Authorizer` | A bridge to a host policy engine |
| Store and blob store | `Config.Store`, `Config.Blobs` | An alternative backend |

The public packages are `workspace`, `kernel`, `content`, `prompt`, `provider`, `agent`, `blob`, `store` and `domain`. They follow semantic versioning from v1.0, and everything else lives under `internal/`. Until v1.0 the module is v0.x and may change between minors.

## 14. Hosting

The workspace runs standalone as a library, and the `workspace-hestia` module hosts it inside hestia without the core ever importing hestia. The adapter is a separate Go module that imports both and translates between them.

Standalone use needs three things: an anansi persistence opened on a database path, `anansistore.New` over it, and `workspace.Open`. Everything then runs in-process.

In hestia, the adapter implements hestia's module contract. It resolves the persistence, logger and config from hestia's DI container, builds the workspace on them, and registers the stream messages described below. The design below works around what the hestia source actually does.

| Concern | Adapter design | hestia behaviour it accounts for |
| --- | --- | --- |
| Streams | All workspace traffic uses two paired messages: a command stream (NDJSON input) and an event stream (SSE output). The client opens both. Each command carries a correlation ID, and its outcome returns as an event. | hestia has no duplex message. An input stream yields one final result, and the adapter drains the input when the handler returns. |
| Opening a stream | The stream message's policy answers one question: may this user use a workspace. hestia checks it once, when the stream opens. | The secure dispatcher resolves a rule key per message and evaluates it once per message, not per item. |
| Resource access | The workspace authorizer forwards each command's resource check to hestia. It resolves a rule key with the permission manager and calls the access controller with the resource, which is the owner and the conversation ID. | Items inside a stream never pass through the dispatcher, so the adapter calls the same two primitives the secure dispatcher uses. |
| Naming | Rule keys use the feature name `conversation`, never `session`. | `session` already means a login session in hestia. |
| Identity | Map hestia's claims to `kernel.Principal`, with the user ID as owner. | Tenancy is parked in hestia, so isolation rests on workspace owner scoping. |
| Persistence | Reuse the persistence from the container, with `ws_`-prefixed collections and workspace migrations run at module start. | hestia core ships no default persistence; the host app opts in. |
| Events | The event stream bridges the bus to a result channel with a bounded buffer. A send blocked past a timeout counts as a disconnect and drops the subscriber. | hestia's HTTP layer stops forwarding on disconnect but never signals the producer. |
| Cancellation | An explicit run-cancel command on the command stream. The run manager owns the run's context, never the request. | Request contexts do not cancel when a client disconnects. |
| Durability | Runs resume from stored checkpoints at module start. Fire-and-forget dispatch is not used for runs. | Fire-and-forget work is lost if the process exits before it runs. |
| Blobs | A blob directory under the host's data directory, through the blob port. | hestia's own blob messages are namespaced and chunked, and are not reused. |
| Output schemas | The event stream declares an output schema, and each event is a document. | hestia sanitizes outgoing documents with per-feature field masks, to be verified in Phase 0. |
| Clients that cannot stream | A unary message with the same envelope and the same authorizer. | Browsers can stream request bodies only over HTTP/2 or HTTP/3, and the Wails interface works differently. Phase 0 checks both. |
| Desktop | A Wails shell with the hestia TypeScript SDK for the UI. | The SQLite driver needs CGo, so builds need a per-OS toolchain matrix. |

DTOs follow hestia's own pattern, which comes from anansi's schema-first model. The schedules feature in the hestia source is the example to copy.

- **Input DTOs.** One struct per message, with `input` tags that point into the message's sections, for example `payload.title` or `arguments.id`. hestia binds the pooled input document into the struct, releases it, and derives the message's input schema from the struct.
- **Output DTOs.** View structs that embed `DocumentModel` and carry `anansi` tags. The output schema is derived from the struct, and each feature declares its sanitization rules as field masks.
- **Lists.** hestia's own services return persistence documents directly for lists, with no mapping. The workspace cannot, because the store port hides documents, so list messages map domain types to views with generated mappers.
- **Where they live.** DTO structs, mappers and annotations live only in `workspace-hestia`, so the core carries no hestia tags.

For streams, the input DTO is one envelope struct with `input` tags for the correlation ID, the command name and the body. hestia binds only the envelope. The workspace then decodes the body with the command's own anansi schema and validates it, so hestia never needs to know individual commands. Authorization rule keys are `workspace:<feature>:<scope>:<action>` names derived from the command names, and Phase 0 checks whether they can be seeded as policy bindings without routed handlers.

The stream messages replace per-command messages. Every kernel command from section 7 travels as an envelope, so hestia needs three messages:

| Message | Kind | Carries |
| --- | --- | --- |
| `workspace:stream:commands:send` | Input stream, NDJSON | Command envelopes with a correlation ID, a command name and a payload. The final result summarizes counts and failures. |
| `workspace:stream:events:subscribe` | Output stream, SSE | Domain events, plus `command.completed` and `command.failed` outcomes with their issues |
| `workspace:command:unary:send` | Request and response | One command envelope, for clients that cannot stream |

hestia's policy engine decides once, when a stream opens: may this user use a workspace. The workspace authorizer then decides per command and forwards each resource check to hestia's permission manager and access controller. Owner scoping in the store remains a second, independent wall.

## 15. Cross-cutting concerns

Seven concerns apply to every package: configuration, errors, security, observability, testing, performance and versioning.

**Configuration.** Options are plain Go structs validated at `Open`, with no global state. Loading from files or the environment is the host's job. Defaults:

| Option | Default | Meaning |
| --- | --- | --- |
| `MaxSteps` | 25 | Agent steps per run |
| `MaxParallelTools` | 4 | Concurrent tool calls in one step |
| `RunDeadline` | 10 min | Wall-clock limit per run |
| `RunCostCapMicros` | 0 (off) | Cap per run in micro-KES |
| `TxTimeout` | 5 s | Store transaction timeout |
| `EventBuffer` | 64 | Subscriber buffer size |
| `HeartbeatInterval` | 10 s | Run heartbeat. A run is stale after three missed beats. |
| `SweepInterval` | 1 h | Maintenance interval. 0 disables it. |
| `SweepGrace` | 24 h | Age before unreachable data is removed |
| `MaxValueBytes` | 8 MB | Largest stored payload |

**Errors.** Every error is an anansi `*common.SystemError` wrapping its cause, so `errors.Is` and `errors.As` work through every layer; there is no second error type in the core. Detail belongs in `Operation`, `Path`, `Issues` and `Retryable` rather than in message text, so a host can log, branch and retry on structure instead of parsing strings. Packages return errors and do not panic, except for programmer mistakes such as registering a nil handler.

**Security.** The main threats and their mitigations:

| Threat | Mitigation |
| --- | --- |
| Cross-owner access | Owner-scoped repositories and the authorizer, with tests on every implementation |
| Prompt injection through documents or tool results | They stay in their own blocks and never enter system sections. Agents run with narrower scopes than their users. High-risk tools need approval. Tool output size is capped. |
| Secret leakage | Credentials are never stored, and logs never include payloads. A redaction hook covers custom fields. |
| Resource abuse | Caps on payload size, steps, parallel tools, run time, run cost and subscriber buffers |
| Supply chain | Pinned versions, `go mod verify` and `govulncheck` in CI, and review of alpha dependencies (anansi, hestia) at each upgrade |

**Observability.** Logs use `log/slog`. Traces use OpenTelemetry with spans named `kernel.dispatch`, `store.<operation>`, `prompt.build`, `provider.stream`, `agent.step` and `tool.call`. Metrics cover command latency, transaction duration, conflicts, runs by status, tokens in and out, cost in micro-KES, provider retries and dropped subscribers. Attributes never carry payloads.

**Testing.** The full setup is in section 16. In brief: unit tests per package. Property tests for the turn tree. Golden tests for assembled prompts. The store conformance and differential suites. Fuzzing for cursors, block parsing and the SSE decoder. End-to-end tests on the replay provider. Failure-injection tests that kill a run at every checkpoint. The race detector runs in CI, and benchmarks are tracked over time.

**Performance.** Targets are in section 3. The rules behind them: no unbounded loads, no payload logging on hot paths, events never buffered beyond a subscriber's buffer, and every pooled anansi document released before a method returns.

**Versioning.** The core follows semantic versioning. Stored schemas migrate forward only. Command, event, message and block type names are stable once released, and deprecations are announced one minor version ahead. The minimum Go version is 1.27, which anansi requires.

**Distribution.** The core is a Go module, and an application ships as one binary per platform. SQLite needs CGo, so each target needs a C toolchain in CI, while the in-memory backend needs none. The license decides who can embed the library: go-anansi is AGPL with a commercial option, so closed-source embedders need that option.

## 16. Test setup

Every complex dependency sits behind an interface with a fake, so the core is data management that runs in-process in milliseconds and can be tested exhaustively. Four rules keep it that way:

1. No test needs a network, a real clock or a real database. The exceptions are the contract runs on anansi and the optional live tests.
2. Time and IDs are injected. Nothing in the core calls `time.Now`, reads a random source or depends on map order.
3. Every port has a contract suite that any implementation, real or fake, must pass.
4. Tests wait on events, never on sleeps.

| Layer | What it covers | Doubles used | When it runs |
| --- | --- | --- | --- |
| Unit | Pure logic per package: turn tree, budget, parsing, serializer | None | Every commit |
| Property | Turn-tree and store invariants against a reference model | Memory store | Every commit |
| Contract | `storetest`, `blobtest` and `providertest` against each implementation | The implementation under test | Every commit for memory, anansi on its ephemeral backend and fakes; CI for anansi on SQLite |
| Golden | Assembled prompts and provider requests | Scripted provider | Every commit |
| Integration | Kernel, store and agent loop together | The harness: memory store, scripted provider, fake clock | Every commit |
| Failure injection | A crash at every commit and checkpoint | Faulty store and harness restart | CI |
| Differential | Memory store against anansi on random operation sequences | Both stores | CI |
| Adapter end to end | The hestia dispatcher with a replay provider on a temporary SQLite file | Real anansi | CI |
| Live contract | Real provider APIs | Real keys, build tag `live` | Manual or nightly |

The `workspacetest` package holds the doubles:

```go
package workspacetest

type FakeClock struct {
	// unexported fields
}

func NewFakeClock(start time.Time) *FakeClock

func (c *FakeClock) Now() time.Time

func (c *FakeClock) Advance(d time.Duration)

type SeqIDs struct {
	// unexported fields
}

func NewSeqIDs(prefix string) *SeqIDs

func (g *SeqIDs) NewID() string

type ScriptedProvider struct {
	// unexported fields
}

func NewScriptedProvider(name string) *ScriptedProvider

func (p *ScriptedProvider) ReplyText(text string) *ScriptedProvider

func (p *ScriptedProvider) ReplyToolCalls(calls ...provider.ToolCallPart) *ScriptedProvider

func (p *ScriptedProvider) FailWith(err error) *ScriptedProvider

func (p *ScriptedProvider) Requests() []provider.Request

type FaultPlan struct {
	FailBeforeCommit map[string]error // command or checkpoint name to the error returned
	CrashAfterCommit map[string]bool  // names that simulate a crash right after the commit
}

func NewFaultyStore(inner store.Store, plan FaultPlan) store.Store

type EventLog struct {
	// unexported fields
}

func (l *EventLog) WaitFor(ctx context.Context, name string) (kernel.EventEnvelope, error)

func (l *EventLog) All() []kernel.EventEnvelope

type harnessConfig struct {
	// unexported fields
}

type HarnessOption func(*harnessConfig)

type Harness struct {
	WS       *workspace.Workspace
	Store    store.Store
	Blobs    blob.Store
	Clock    *FakeClock
	Provider *ScriptedProvider
	Events   *EventLog
}

func NewHarness(t testing.TB, opts ...HarnessOption) *Harness

func (h *Harness) As(owner string) *workspace.Handle

func (h *Harness) Restart(t testing.TB) // abandons the workspace without Close, then reopens it on the same store
```

The package also holds a `ReplayProvider` that serves recorded transcripts, a programmable `FakeTool` that counts its calls, and an in-memory blob store.

Each contract suite exposes one function: `storetest.Run(t, newStore)`, `blobtest.Run(t, newStore)` and `providertest.Run(t, newProvider)`. The factory returns a fresh instance for every subtest. The memory store starts instantly, and anansi runs on a SQLite file under `t.TempDir()`.

Test data comes from three places:

- **TS fixtures** in `testdata/ts-fixtures/`, generated by the Bun harness described in the Sources and agent brief tab. Where Go intentionally differs, the fixture carries a divergence note and the Go test asserts the intended result.
- **Golden files** in `testdata/golden/`, regenerated with `go test -update` and reviewed like code. They must be byte-stable.
- **Replay transcripts** in `testdata/replays/`, recorded once against the real APIs by a small `examples/record` tool and stripped of secrets.

Failure injection works in-process. `FaultyStore` fails, or simulates a crash, at named points: before or after the commit of each command, and at each run checkpoint. A crash abandons the running workspace without closing it, and `Restart` reopens a workspace on the same store and runs recovery. The test then asserts history, run state and tool-call counts. One chaos test, under the `chaos` build tag, runs the workspace in a subprocess and kills it with SIGKILL.

CI gates:

- `go vet`, `staticcheck` and `govulncheck` must pass.
- `go test -race -shuffle=on -count=1 ./...` must pass, with a goroutine-leak check in each package's `TestMain`.
- Coverage floors are 90% for `kernel` and `store` and 85% for the other core packages.
- An import-boundary test applies the dependency rules from section 5 using `go list -deps`.
- Fuzz targets for cursors, block parsing, the SSE decoder and chain loading run for 30 seconds each nightly, and their seed corpora run on every commit.
- Benchmarks for the section 3 targets are tracked with `benchstat`, and a regression over 20% fails the nightly run.
- The example in section 13 is a runnable `Example` test, so it cannot rot.

Make targets: `make test` (unit, property, integration and memory contracts, under 60 seconds), `make test-anansi`, `make fuzz`, `make bench`, `make chaos`, `make live` and `make golden`.

A package is done when its unit or contract tests exist, it passes under `-race`, its public API has an `Example`, and its coverage meets the floor.

## 17. Delivery plan

The work runs in seven phases, and each phase ends at a gate that must pass before the next one starts.

1. **Phase 0, spike and fixtures.** Pin versions. Build the TS fixture harness in a Bun project set up as described in the Sources and agent brief tab and dump golden fixtures for turn-tree cases, prompt assembly and block parsing. Verify the runtime unknowns in section 15 of Store specification, plus hestia's disconnect and sanitization behaviour, streaming request bodies from the clients, policy bindings for inner commands, and `asaidimu/blobs` v1.3.x. Also profile the TS library on a 500-turn conversation, which is the baseline for the Go performance targets. Gate: a go or no-go note, an upstream ticket list, committed fixtures and the TS baseline.
2. **Phase 1, kernel and domain on the memory store.** Commands, units of work, serializer, authorizer, events, turn tree. Gate: property tests and kernel invariants pass under `-race`, and the memory store passes the conformance suite.
3. **Phase 2, anansi store and blobs.** Gate: the same conformance suite passes on `anansistore`, the differential test is green, and the blob reconcile job works.
4. **Phase 3, registry, pipeline and providers.** Gate: golden prompts match, recorded-HTTP tests pass for every adapter, and streaming and cancel tests pass on the replay provider.
5. **Phase 4, agent loop and tools.** Gate: failure-injection tests pass at every checkpoint, the approval flow works, and step and cost caps hold.
6. **Phase 5, public API, reference CLI and hestia adapter.** Gate: scenarios S1 to S8 pass through the CLI and through hestia's dispatcher, including client disconnect and cancel.
7. **Phase 6, hardening and release.** License alignment, security review, benchmarks against the section 3 targets, documentation and a v0.1 tag.

The main risks:

| Risk | Impact | Mitigation |
| --- | --- | --- |
| anansi v8 and hestia are alpha and change under us | Rework | Pin versions, keep adapters thin, and turn spike findings into upstream tickets, since both are ours |
| anansi lacks a capability we assumed: transaction hooks, update counts, raw recursive queries | Store design change | Phase 0 verifies each one, and fallback paths are already specified |
| SQLite's single writer stalls many conversations | Latency | No slow I/O in transactions, short timeouts, measured in Phase 0, other backends later through anansi |
| CGo complicates desktop builds | Slower releases | A per-OS CI matrix from Phase 0, and a look at pure-Go drivers |
| Provider APIs drift | Breakage | Thin adapters and replay tests built from recorded transcripts |
| Building every TS README promise | Delay | Build only what this spec defines. Retrieval and compaction get simple defaults, nothing more. |
| The workspace is a strong substrate but not yet a complete agent harness: context compaction, MCP, sandboxed tools and sub-agents are thin or absent | Harness claims go unproven | Dogfood one real loop on it, compare it with an existing harness on a fixed task set, and add those pieces as layers on top |
| hestia's README says MIT while its LICENSE.md and pkg.go.dev say AGPL, and anansi is AGPL with a commercial option | Adoption confusion, and AGPL limits embedding in closed products | Choose one license for the core and align every repository before the first release |

Decisions taken so far:

| # | Decision | Why |
| --- | --- | --- |
| D1 | A standalone library, hosted by hestia through a separate module | The kernel needs state semantics hestia lacks |
| D2 | Its own command kernel with atomic commands through store transactions | Replaces the TS virtual-state saga |
| D3 | anansi behind a store port, with a memory reference store, pending open decision 1 | Backend independence and testability |
| D4 | Turns belong to the owner and conversations hold heads | One-row forks |
| D5 | Native tool calling by default, structured-output mode deferred | Better provider support, and no anansi-to-JSON-Schema translator ships |
| D6 | The core defines a `Tool` interface. Exposing hestia messages as tools comes later, in the adapter. | Keeps the core host-neutral |
| D7 | Blob bytes through `asaidimu/blobs` behind a port | Streaming and deduplication |
| D8 | The domain term is conversation, not session | Avoids hestia's login sessions |
| D9 | Money as integer micro-KES | No floating point |
| D10 | First providers: Anthropic, OpenAI and Gemini, to confirm | Matches the likely users |
| D11 | Two paired hestia streams, commands in and events out, with one policy check when a stream opens and per-command resource checks forwarded to hestia | hestia has no duplex message and evaluates policy once per message |
| D12 | anansi's schema, validation and binding packages may be used in `content`, while persistence stays behind the store adapter | One schema defines the DTO, the validator and the client types |
| D13 | A produced block is committed as typed only after strict validation, with one repair attempt | Correctness is enforced at the boundary and not trusted from the model |

Eleven decisions are still open. Each has a recommendation and the phase that settles it.

| # | Open decision | Recommendation | Settled in |
| --- | --- | --- | --- |
| 1 | Keep the store port, the memory store and the mapping layer, or make the generated anansi models the domain types and the repositories thin services over `ModelCollection` | Collapse them. anansi already ships in-memory and SQLite backends, shapes, a model cache and optimistic locking. The port buys only independence from anansi itself. | Phase 0 |
| 2 | Model writes as emittable blocks with effects, or as tool calls | Tool calls, so writes use the same authorized and audited path | Phase 0 |
| 3 | Typed blocks in the domain and typed pipeline stages, instead of raw JSON and a shared build struct | Adopt both. In the store, keep anansi's rule of a type string plus a payload, never a union. | Phase 1 |
| 4 | Immutability rules, a decoded-turn cache with per-model token counts, and frozen registries | Adopt, so each agent step extends the chain instead of reloading it | Phase 1 |
| 5 | Typed enums instead of string kinds, slices instead of map bags, a sharded mutex instead of a FIFO serializer | Adopt | Phase 1 |
| 6 | Whether a long-lived streaming request body works from the desktop and browser clients | Test it, and make the unary message the default if it does not | Phase 0 |
| 7 | How to seed policy bindings for command names that are not routed messages, and how API-key allowlists apply inside a stream | Test `EnsureBinding` first | Phase 0 |
| 8 | Tool schema route: authored JSON Schema, reflection from the input type, or a translator from anansi schemas | Reflection from the input type, validated with anansi's JSON Schema compiler | Phase 0 |
| 9 | Whether anansi exposes commit and rollback hooks inside its transaction callback | Verify, with the fallback paths already specified | Phase 0 |
| 10 | Which providers ship first (D10) | Anthropic, OpenAI and Gemini | Phase 3 |
| 11 | The module path and the license for the core | Decide one license and align every repository | Before Phase 6 |

## 18. Appendix: what changes from the TS library

Fifteen behaviours change, so this is a rewrite and not a port. The TS findings come from reading its types and bundled code, not from running it.

| TS behaviour | Go behaviour | Reason |
| --- | --- | --- |
| State plus a hand-patched index, merged with deep-cloned patches | Queries read the store. No shared mutable state. | Removes stale-index bugs and cloning cost |
| One global promise-chain lock | A per-conversation serializer plus store compare-and-swap | Unrelated conversations stop blocking, and it works across processes |
| Sagas on a virtual state copy, with undo commands | One store transaction per command, with compensation hooks for external effects | In TS only in-memory state was speculative, while durable writes were immediate |
| README describes a DAG, code is a versioned tree | A versioned tree with owner-scoped turns | An accurate model with one-row forks |
| Fork copied the head, but turns were filtered by session | Fork creates a conversation pointing at an existing turn | A forked session appeared empty |
| Deleting a session removed only its record | Delete plus a reachability sweep and holder links | No orphaned turns or blobs |
| Releasing a blob never purged it at zero | Holder links, orphan detection and sweeps | A defined lifecycle |
| Topic diffs looked at an entity's first topic only | Exact-match topic semantics, tested | Multi-topic entities were mishandled |
| Tools emitted as JSON blocks through structured output | Hybrid by default: prose plus `workspace:<type>` fences with native tool calls; JSON-only structured output as a fallback | Better provider support, and typed blocks the model can interleave with prose |
| No agent loop | An agent loop with limits, approval and checkpoints | Missing feature |
| One non-streaming Google call, token counts of zero | Streaming everywhere, usage and cost per turn in KES | Missing feature |
| Retrieval, conflict resolution and compaction as interfaces only | Simple defaults behind the same interfaces | The README promised them and the code did not deliver |
| Prices as USD floats | Integer micro-units, converted to KES | Safe money arithmetic |
| Called a session | Called a conversation | Avoids hestia's login sessions |
| A `Result` wrapper type for errors | Idiomatic `(T, error)` with typed errors | Go idiom |
