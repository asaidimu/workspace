// Package workspace is the embeddable facade: Open returns a Workspace, and
// For(principal) returns a Handle whose methods are scoped to that
// principal's owner. Everything else on the package is an extension point.
//
// This is the one place the adapters are wired together (Workspace
// specification, section 5). Nothing else in the core imports an adapter.
package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/blob"
	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/prompt"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// Clock and IDGenerator are aliases of the domain interfaces, so a host can
// inject them without importing domain.
type Clock = domain.Clock
type IDGenerator = domain.IDGenerator

// Config is everything Open needs. Store, Blobs, Providers and Authorizer
// are required; the rest have defaults.
type Config struct {
	Store       store.Store
	Blobs       blob.Store
	Providers   []provider.Provider
	Credentials provider.CredentialSource
	Authorizer  kernel.Authorizer
	// Registry validates block types on turn writes. Open defaults it to
	// the built-in types of section 8; a host replaces or extends them.
	Registry content.Registry
	Pipeline *prompt.Pipeline
	Tools    []agent.Tool
	Clock    Clock
	IDs      IDGenerator
	Logger   *slog.Logger
	Options  Options
}

// Options are the workspace settings from Workspace specification section
// 15. The zero value means "every default in the table".
type Options struct {
	// MaxSteps bounds an agent run's steps (25).
	MaxSteps int
	// MaxParallelTools bounds concurrent tool calls in one step (4).
	MaxParallelTools int
	// RunDeadline bounds one run's wall clock (10 min).
	RunDeadline time.Duration
	// RunCostCapMicros caps one run's cost in micro-KES (0, off).
	RunCostCapMicros int64
	// TxTimeout bounds each command's transaction (5 s).
	TxTimeout time.Duration
	// EventBuffer is the default subscriber buffer size (64).
	EventBuffer int
	// HeartbeatInterval is the run heartbeat (10 s); a run is stale after
	// three missed beats.
	HeartbeatInterval time.Duration
	// SweepInterval is the maintenance cadence (1 h). It is a pointer so
	// the section 15 rule "0 disables it" is expressible: nil selects the
	// default, a non-nil zero turns sweeping off.
	SweepInterval *time.Duration
	// SweepGrace is the age unreachable data must reach before a sweep
	// removes it (24 h).
	SweepGrace time.Duration
	// MaxValueBytes is the largest stored payload (8 MB). Adapters enforce
	// it; it is recorded here as the configuration of record.
	MaxValueBytes int64
	// Policy is the fixed operating rules the prompt carries in its
	// always-present policy section. Empty omits the section.
	Policy string
}

// The section 15 defaults, named so Options.withDefaults reads as the table.
const (
	defaultMaxSteps          = 25
	defaultMaxParallelTools  = 4
	defaultRunDeadline       = 10 * time.Minute
	defaultTxTimeout         = 5 * time.Second
	defaultEventBuffer       = 64
	defaultHeartbeatInterval = 10 * time.Second
	defaultSweepInterval     = time.Hour
	defaultSweepGrace        = 24 * time.Hour
	defaultMaxValueBytes     = 8 << 20
	defaultRunCostCapMicros  = 0
)

// withDefaults fills every unset option with its section 15 default.
func (o Options) withDefaults() Options {
	if o.MaxSteps <= 0 {
		o.MaxSteps = defaultMaxSteps
	}
	if o.MaxParallelTools <= 0 {
		o.MaxParallelTools = defaultMaxParallelTools
	}
	if o.RunDeadline <= 0 {
		o.RunDeadline = defaultRunDeadline
	}
	if o.RunCostCapMicros < 0 {
		o.RunCostCapMicros = defaultRunCostCapMicros
	}
	if o.TxTimeout <= 0 {
		o.TxTimeout = defaultTxTimeout
	}
	if o.EventBuffer <= 0 {
		o.EventBuffer = defaultEventBuffer
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = defaultHeartbeatInterval
	}
	if o.SweepInterval == nil {
		d := defaultSweepInterval
		o.SweepInterval = &d
	}
	if o.SweepGrace <= 0 {
		o.SweepGrace = defaultSweepGrace
	}
	if o.MaxValueBytes <= 0 {
		o.MaxValueBytes = defaultMaxValueBytes
	}
	return o
}

// Workspace is an opened workspace: the wired kernel, stores and managers.
type Workspace struct {
	store      store.Store
	blobs      blob.Store
	k          *kernel.Kernel
	opts       Options
	clock      domain.Clock
	ids        domain.IDGenerator
	manager    agent.RunManager
	reconciler *blob.Reconciler
	blocks     content.Registry
}

// Open wires the adapters together and returns a ready workspace. Everything
// it validates is a configuration mistake: a missing dependency, an empty
// provider list, a tool registered twice. Nothing here touches the network.
func Open(ctx context.Context, cfg Config) (*Workspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, kernel.FromStore(err)
	}
	if cfg.Store == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "a store is required")
	}
	if cfg.Blobs == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "a blob store is required")
	}
	if len(cfg.Providers) == 0 {
		return nil, kernel.NewError(kernel.CodeInvalid, "at least one provider is required")
	}
	if cfg.Authorizer == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "an authorizer is required; use kernel.AllowOwner() for single-user apps")
	}

	opts := cfg.Options.withDefaults()
	clock := cfg.Clock
	if clock == nil {
		clock = kernel.DefaultClock()
	}
	ids := cfg.IDs
	if ids == nil {
		ids = kernel.DefaultIDs()
	}

	kopts := []kernel.Option{
		kernel.WithAuthorizer(cfg.Authorizer),
		kernel.WithClock(clock),
		kernel.WithIDs(ids),
		kernel.WithTxTimeout(opts.TxTimeout),
	}
	if cfg.Logger != nil {
		kopts = append(kopts, kernel.WithLogger(cfg.Logger))
	}
	k, err := kernel.New(cfg.Store, kopts...)
	if err != nil {
		return nil, err
	}

	pipeline := cfg.Pipeline
	if pipeline == nil {
		pipeline = prompt.DefaultPipeline()
	}

	registry := agent.NewRegistry()
	for _, tool := range cfg.Tools {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}

	blocks := cfg.Registry
	if blocks == nil {
		blocks = content.NewRegistry()
	}

	runner, err := agent.NewStepper(agent.StepperConfig{
		History:  &history{store: cfg.Store, clock: clock, ids: ids},
		Pipeline: pipeline,
		Provider: &providers{list: cfg.Providers},
		Registry: registry,
		Clock:    clock,
		IDs:      ids,
		Policy:   opts.Policy,
		Content:  blocks,
		Blobs:    blobResolver{w: &Workspace{blobs: cfg.Blobs, store: cfg.Store}},
		Effects:  kernelEffects{k},
	})
	if err != nil {
		return nil, err
	}

	manager, err := agent.NewManager(agent.ManagerConfig{
		Store:     cfg.Store,
		Stepper:   runner,
		Resolver:  runner,
		Emitter:   k,
		Clock:     clock,
		IDs:       ids,
		Heartbeat: opts.HeartbeatInterval,
	})
	if err != nil {
		return nil, err
	}

	// The reconcile job removes bytes whose checksum has no metadata under
	// any owner; the metadata check runs in system scope (Workspace
	// specification, section 12).
	lookup := blob.LookupFunc(func(ctx context.Context, checksum string) (bool, error) {
		repos, err := cfg.Store.Scoped(store.Scope{System: true})
		if err != nil {
			return false, err
		}
		_, err = repos.Blobs().GetByChecksum(ctx, checksum)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	})

	w := &Workspace{
		store:      cfg.Store,
		blobs:      cfg.Blobs,
		k:          k,
		opts:       opts,
		clock:      clock,
		ids:        ids,
		manager:    manager,
		reconciler: blob.NewReconciler(cfg.Blobs, lookup, clock.Now),
		blocks:     blocks,
	}
	if err := w.registerCommands(); err != nil {
		return nil, err
	}
	return w, nil
}

// For returns a handle scoped to the principal's owner. Every method on the
// handle runs as that principal; a handle never widens it.
func (w *Workspace) For(p kernel.Principal) *Handle {
	return &Handle{w: w, p: p}
}

// Events returns the workspace's event bus. Subscribing with a non-positive
// buffer asks for Options.EventBuffer.
func (w *Workspace) Events() kernel.Bus {
	return bufferedBus{inner: w.k.Bus(), buffer: w.opts.EventBuffer}
}

// Admin returns the maintenance surface: sweeps, migrations and health.
func (w *Workspace) Admin() Admin { return &admin{w: w} }

// Close releases the workspace's resources. It does not close the caller's
// store or blob store, and it does not abandon runs in flight: they finish
// or fail on their own, and a host that must stop them calls Cancel first.
func (w *Workspace) Close(ctx context.Context) error { return ctx.Err() }

// bufferedBus substitutes the workspace's configured subscriber buffer when
// a caller passes none, so Options.EventBuffer is the default everyone gets.
type bufferedBus struct {
	inner  kernel.Bus
	buffer int
}

// kernelEffects is the agent.EffectsDispatcher port over the workspace
// kernel: it dispatches a block's command through the whole pipeline keyed
// with the block index, so a replay converges on one stored receipt.
type kernelEffects struct{ k *kernel.Kernel }

func (e kernelEffects) DispatchEffect(ctx context.Context, p kernel.Principal, cmd kernel.Command, idempotencyKey string) error {
	_, err := kernel.DispatchAny(ctx, e.k, p, cmd, kernel.WithIdempotencyKey(idempotencyKey))
	return err
}

func (b bufferedBus) Subscribe(ctx context.Context, f kernel.EventFilter, buffer int) (<-chan kernel.EventEnvelope, error) {
	if buffer <= 0 {
		buffer = b.buffer
	}
	return b.inner.Subscribe(ctx, f, buffer)
}

// providers routes a request to the first configured provider that claims
// the model. With one provider — the common case — it is a pass-through.
type providers struct {
	list []provider.Provider
}

func (p *providers) pick(model string) (provider.Provider, error) {
	if len(p.list) == 0 {
		return nil, kernel.NewError(kernel.CodeUnavailable, "no providers are configured")
	}
	if len(p.list) == 1 {
		return p.list[0], nil
	}
	var firstErr error
	for _, candidate := range p.list {
		_, err := candidate.Capabilities(model)
		if err == nil {
			return candidate, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, kernel.WrapError(kernel.CodeUnavailable, "no provider serves model "+model, firstErr)
}

func (p *providers) Name() string {
	if len(p.list) == 1 {
		return p.list[0].Name()
	}
	return "providers"
}

func (p *providers) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	var out []provider.ModelInfo
	for _, candidate := range p.list {
		models, err := candidate.Models(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, models...)
	}
	return out, nil
}

func (p *providers) Capabilities(model string) (provider.Capabilities, error) {
	candidate, err := p.pick(model)
	if err != nil {
		return provider.Capabilities{}, err
	}
	return candidate.Capabilities(model)
}

func (p *providers) Generate(ctx context.Context, req provider.Request) (provider.Response, error) {
	candidate, err := p.pick(req.Model)
	if err != nil {
		return provider.Response{}, err
	}
	return candidate.Generate(ctx, req)
}

func (p *providers) Stream(ctx context.Context, req provider.Request) (provider.Stream, error) {
	candidate, err := p.pick(req.Model)
	if err != nil {
		return nil, err
	}
	return candidate.Stream(ctx, req)
}

func (p *providers) UploadFile(ctx context.Context, src provider.Source, name string) (provider.RemoteFile, error) {
	if len(p.list) == 0 {
		return provider.RemoteFile{}, kernel.NewError(kernel.CodeUnavailable, "no providers are configured")
	}
	// A file upload is not model-scoped, so it goes to the first provider.
	return p.list[0].UploadFile(ctx, src, name)
}

// Handle is the owner-scoped API. Every method on it runs as the principal
// it was created with.
type Handle struct {
	w *Workspace
	p kernel.Principal
}

func (h *Handle) Conversations() Conversations { return &conversations{scoped{w: h.w, p: h.p}} }
func (h *Handle) Turns() Turns                 { return &turns{scoped{w: h.w, p: h.p}} }
func (h *Handle) Runs() Runs                   { return &runs{scoped{w: h.w, p: h.p}} }
func (h *Handle) Content() Content             { return &contents{scoped{w: h.w, p: h.p}} }
func (h *Handle) Roles() Roles                 { return &roles{scoped{w: h.w, p: h.p}} }
func (h *Handle) Blobs() Blobs                 { return &blobs{scoped{w: h.w, p: h.p}} }
func (h *Handle) Settings() Settings           { return &settings{scoped{w: h.w, p: h.p}} }

// Conversations creates, reads, lists, updates, forks and deletes
// conversations.
type Conversations interface {
	Create(ctx context.Context, in CreateConversation) (domain.Conversation, error)
	Get(ctx context.Context, id string) (domain.Conversation, error)
	List(ctx context.Context, f store.ConversationFilter, p store.PageRequest) (store.Page[domain.Conversation], error)
	Update(ctx context.Context, id string, in UpdateConversation) (domain.Conversation, error)
	Fork(ctx context.Context, id string, at domain.TurnRef, title string) (domain.Conversation, error)
	Delete(ctx context.Context, id string) error
}

// Turns appends, edits, switches and walks the turn tree.
type Turns interface {
	Append(ctx context.Context, conversationID string, in AppendTurn) (domain.Turn, error)
	Edit(ctx context.Context, conversationID string, ref domain.TurnRef, in EditTurn) (domain.Turn, error)
	Switch(ctx context.Context, conversationID string, ref domain.TurnRef) (domain.Conversation, error)
	Chain(ctx context.Context, conversationID string, opts store.ChainOptions) ([]domain.Turn, error)
	Branches(ctx context.Context, conversationID string, ref domain.TurnRef) (BranchInfo, error)
}

// Runs starts, reads, cancels and approves agent runs.
type Runs interface {
	Start(ctx context.Context, conversationID string, in StartRun) (domain.Run, error)
	Get(ctx context.Context, runID string) (domain.Run, error)
	Cancel(ctx context.Context, runID string) error
	Approve(ctx context.Context, runID, callID string, approve bool, reason string) error
}

// Content puts, reads, lists and tombstones content versions.
type Content interface {
	Put(ctx context.Context, c domain.Content, baseVersion int) (domain.Content, error)
	Get(ctx context.Context, ref domain.ContentRef) (domain.Content, error)
	List(ctx context.Context, f store.ContentFilter, p store.PageRequest) (store.Page[domain.Content], error)
	Tombstone(ctx context.Context, key string, baseVersion int) error
}

// Roles puts, reads, lists and deletes roles.
type Roles interface {
	Put(ctx context.Context, r domain.Role, expectedRev int64) (domain.Role, error)
	Get(ctx context.Context, id string) (domain.Role, error)
	List(ctx context.Context, f store.RoleFilter, p store.PageRequest) (store.Page[domain.Role], error)
	Delete(ctx context.Context, id string, expectedRev int64) error
}

// Settings is the workspace-wide default surface: the prompt fallback, the
// language, the default role and the global preference keys.
type Settings interface {
	Get(ctx context.Context) (domain.Settings, error)
	Update(ctx context.Context, in UpdateSettings) (domain.Settings, error)
}

// Blobs uploads bytes and opens them by ID.
type Blobs interface {
	Upload(ctx context.Context, r io.Reader, opts UploadOptions) (domain.Blob, error)
	Open(ctx context.Context, id string) (io.ReadSeekCloser, error)
}

// Admin is the maintenance surface.
type Admin interface {
	Sweep(ctx context.Context) (SweepSummary, error)
	Migrate(ctx context.Context) error
	Health(ctx context.Context) (Health, error)
}

// UploadOptions carries the media type and name for an upload.
type UploadOptions struct {
	MediaType string
	Name      string
}

// BranchInfo is the sibling versions of a turn and which one is active.
type BranchInfo struct {
	Active   domain.TurnRef
	Siblings []domain.TurnRef
}

// SweepSummary reports what one admin sweep removed. Bytes counts orphan
// byte files deleted by the reconcile pass; Blobs counts metadata records
// removed by the maintenance sweeps.
type SweepSummary struct {
	Turns    int
	Blobs    int
	Receipts int
	Bytes    int
}

// Health is the workspace's self-report.
type Health struct {
	OK      bool
	Store   string
	Version int
}

// LocalUser builds a principal holding every scope, for single-user apps
// that open a workspace with kernel.AllowOwner().
func LocalUser(owner string) kernel.Principal {
	return kernel.Principal{Owner: owner, Subject: owner, Kind: kernel.UserPrincipal}
}

// Text builds a text block, the common case for AppendTurn and EditTurn.
// The value is the {"text": ...} object the block port and the prompt
// pipeline both read; a bare string would be stored but never reach the
// model.
func Text(s string) []domain.Block {
	return []domain.Block{{Type: "text", Value: map[string]any{"text": s}}}
}
