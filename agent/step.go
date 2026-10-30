package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/prompt"
	"github.com/asaidimu/workspace/provider"
)

// Defaults for one step and one run, from section 11: up to four tool calls
// per step, 25 steps and a 10-minute deadline per run.
const (
	DefaultMaxSteps    = 25
	DefaultMaxParallel = 4
	DefaultDeadline    = 10 * time.Minute
)

// History is the durable conversation state a step reads and appends to. It
// is a port rather than a direct store dependency because section 11 puts
// authorization, validation, events and idempotency on the step: the facade
// implements it by dispatching through the kernel, so a turn append behaves
// like any other command, and a test implements it directly over
// store/memory. agent never reaches for the turn repository itself.
type History interface {
	// Read takes the consistent snapshot the pipeline needs.
	Read(ctx context.Context, p kernel.Principal, conversationID string) (prompt.Snapshot, error)
	// Append commits one turn to a conversation in one transaction: the
	// turn lands and the conversation's head moves to it, so a restart never
	// sees a turn without the head that points at it. A non-empty
	// idempotency key deduplicates a retry of the same append within the
	// receipt TTL.
	Append(ctx context.Context, p kernel.Principal, conversationID string, turn domain.Turn, idempotencyKey string) (domain.Turn, error)
}

// Prompter is prompt.Pipeline's build entry point, narrowed so a test can
// stub it.
type Prompter interface {
	Build(ctx context.Context, in prompt.Input) (provider.Request, []prompt.Diagnostic, error)
}

// Pricer converts a model's usage into micro-KES. Nil means cost is not
// priced, which caps the cost accounting but never the correctness of a run.
type Pricer func(model string, u provider.Usage) int64

// StepperConfig is what a Stepper needs. History, Pipeline, Provider and
// Registry are required; the rest have working defaults.
type StepperConfig struct {
	History  History
	Pipeline Prompter
	Provider provider.Provider
	Registry Registry
	Pricer   Pricer
	Clock    domain.Clock
	IDs      domain.IDGenerator
	// Policy is the workspace's fixed operating rules, carried into the
	// prompt's policy section when non-empty.
	Policy string
	// Content is the block registry. When set, the pipeline teaches the
	// hybrid block markup and the model's text is parsed into typed blocks;
	// when nil, the legacy static pipeline and mapping are used.
	Content content.Registry
	// Blobs resolves the blob references conversation blocks carry into
	// provider parts. Nil keeps the mappers' placeholders.
	Blobs content.BlobResolver
	// Effects runs the commands committed blocks declare. A registry plus
	// no effects is legal: blocks committed, nothing dispatched.
	Effects EffectsDispatcher
}

type stepper struct {
	hist     History
	pipeline Prompter
	provider provider.Provider
	registry Registry
	pricer   Pricer
	clock    domain.Clock
	ids      domain.IDGenerator
	policy   string
	content  content.Registry
	effects  EffectsDispatcher
	blobs    content.BlobResolver
}

// NewStepper builds the public step primitive: one model call plus the
// execution of the tool calls it asks for, committed in section 11's order.
func NewStepper(cfg StepperConfig) (Runner, error) {
	if cfg.History == nil || cfg.Pipeline == nil || cfg.Provider == nil || cfg.Registry == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "a stepper needs a history, a pipeline, a provider and a registry")
	}
	if cfg.Clock == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "a clock is required; nothing in the core calls time.Now")
	}
	if cfg.IDs == nil {
		return nil, kernel.NewError(kernel.CodeInvalid, "an ID generator is required")
	}
	return &stepper{
		hist:     cfg.History,
		pipeline: cfg.Pipeline,
		provider: cfg.Provider,
		registry: cfg.Registry,
		pricer:   cfg.Pricer,
		clock:    cfg.Clock,
		ids:      cfg.IDs,
		policy:   cfg.Policy,
		content:  cfg.Content,
		effects:  cfg.Effects,
		blobs:    cfg.Blobs,
	}, nil
}

// Step runs one model call and, when the model asks for tools, the tools it
// named. The assistant turn is committed before any tool runs and the tool
// results before the step returns, so a restart repeats at most the calls
// that have no committed result.
func (s *stepper) Step(ctx context.Context, p kernel.Principal, conversationID string, opts StepOptions) (StepResult, error) {
	if conversationID == "" {
		return StepResult{}, kernel.NewError(kernel.CodeInvalid, "a conversation ID is required")
	}
	if opts.Model == "" {
		return StepResult{}, kernel.NewError(kernel.CodeInvalid, "a model is required")
	}
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = DefaultMaxParallel
	}

	snapshot, err := s.hist.Read(ctx, p, conversationID)
	if err != nil {
		return StepResult{}, err
	}

	// Narrowed identity: tools run as an agent principal whose scopes are
	// the user's scopes restricted to what the exposed tools ask for. It can
	// only ever be narrower than p.
	agentP, tools := s.selectTools(p, opts.Tools)

	providerSpecs := make([]provider.ToolSpec, 0, len(tools))
	for _, t := range tools {
		spec := t.Spec()
		providerSpecs = append(providerSpecs, provider.ToolSpec{
			Name: spec.Name, Description: spec.Description, Schema: spec.Schema,
		})
	}

	in := prompt.Input{
		Snapshot:     snapshot,
		Instructions: opts.Instructions,
		Model:        opts.Model,
		Tools:        providerSpecs,
		Policy:       s.policy,
		Content:      s.content,
		Blobs:        s.blobs,
		ProviderName: s.provider.Name(),
		Constraints:  opts.Constraints,
	}
	caps := provider.Capabilities{}
	if c, capErr := s.provider.Capabilities(opts.Model); capErr == nil {
		caps = c
		in.ContextWindow = caps.ContextWindow
		in.MaxOutputTokens = caps.MaxOutputTokens
	}
	in.Capabilities = caps
	in.NativeTools = caps.NativeTools
	req, _, err := s.pipeline.Build(ctx, in)
	if err != nil {
		return StepResult{}, err
	}

	resp, err := s.provider.Generate(ctx, req)
	if err != nil {
		return StepResult{}, err
	}
	// Uncommitted partial text is discarded on cancellation: nothing is
	// written until a complete response has arrived.
	if err := ctx.Err(); err != nil {
		return StepResult{}, kernel.WrapError(kernel.CodeCanceled, "step cancelled before the assistant turn was committed", err)
	}

	now := s.clock.Now()
	blocks, blockDiags := s.parseBlocks(resp.Message.Parts, caps.NativeTools)
	assistant := domain.Turn{
		ID:           s.ids.NewID(),
		Version:      1,
		Owner:        p.Owner,
		Parent:       snapshot.Conversation.Head,
		Actor:        domain.Actor{Kind: domain.ActorAgent, ID: agentP.Subject},
		Blocks:       blocks,
		Usage:        s.usage(opts.Model, resp.Usage),
		FinishReason: string(resp.Finish),
		CreatedAt:    now,
	}
	committed, err := s.hist.Append(ctx, p, conversationID, assistant, "turn:"+assistant.ID)
	if err != nil {
		return StepResult{}, err
	}

	res := StepResult{
		Turns:      []domain.Turn{committed},
		Finish:     resp.Finish,
		Usage:      resp.Usage,
		CostMicros: s.cost(opts.Model, resp.Usage),
	}
	res.Diagnostics = blockDiags

	// The assistant turn is durable; the effects its blocks declare run
	// now, keyed per block so a replay replays one receipt. An effect
	// failure fails the step: the turn stays committed, so a restart
	// re-runs the pending dispatches under a converging key.
	if err := s.dispatchEffects(ctx, p, committed); err != nil {
		return StepResult{}, err
	}
	if resp.Finish != provider.FinishToolCalls {
		return res, nil
	}

	calls := toolCallParts(resp.Message.Parts)
	if len(calls) == 0 {
		// The model claimed tool calls but named none; that is a finished
		// step, not an error the caller must special-case.
		return res, nil
	}
	// A RiskConfirm call pauses for approval and is not executed; only the
	// rest run in this step.
	toRun, pending := splitByRisk(calls, s.registry)
	res.Pending = pending
	if len(toRun) == 0 {
		// Every call is waiting for a person; there is no result turn to
		// commit yet. Approving one resumes the run through the manager.
		return res, nil
	}
	results := s.runTools(ctx, agentP, toRun, opts.MaxParallel)

	// Tool results are not committed once the step is cancelled: the
	// assistant turn stays (it was committed before any tool ran, so the
	// calls can be re-run), and the uncommitted results are discarded.
	if err := ctx.Err(); err != nil {
		return res, kernel.WrapError(kernel.CodeCanceled, "step cancelled before the tool results were committed", err)
	}

	parent := committed.Ref()
	turn := domain.Turn{
		ID:        s.ids.NewID(),
		Version:   1,
		Owner:     p.Owner,
		Parent:    &parent,
		Actor:     domain.Actor{Kind: domain.ActorTool, ID: agentP.Subject},
		Blocks:    resultBlocks(results),
		CreatedAt: s.clock.Now(),
	}
	resultTurn, err := s.hist.Append(ctx, p, conversationID, turn, "turn:"+turn.ID)
	if err != nil {
		return res, err
	}
	res.Turns = append(res.Turns, resultTurn)
	return res, nil
}

// Resolve finishes one pending call, completing the approval half of the
// loop: an approved call runs under the same narrowed identity a fresh step
// would give it — narrowed to that one tool, so approving cannot widen
// anything — and either outcome is committed as a tool-result turn so the
// model is told what happened and can adapt (section 11, approval).
func (s *stepper) Resolve(ctx context.Context, p kernel.Principal, conversationID string, parent domain.TurnRef, call ToolCall, approve bool, reason string) (domain.Turn, error) {
	if call.CallID == "" || call.Name == "" {
		return domain.Turn{}, kernel.NewError(kernel.CodeInvalid, "a pending call needs an ID and a tool name")
	}
	agentP, _ := s.selectTools(p, []string{call.Name})

	var result ToolResult
	if approve {
		result = s.runOne(ctx, agentP, provider.ToolCallPart{
			CallID: call.CallID, Name: call.Name, Arguments: call.Arguments,
		})
	} else {
		if reason == "" {
			reason = "no reason given"
		}
		result = errorResult("tool call rejected: " + reason)
	}
	if err := ctx.Err(); err != nil {
		return domain.Turn{}, kernel.WrapError(kernel.CodeCanceled, "resolution cancelled", err)
	}

	parentRef := parent
	turn := domain.Turn{
		ID:        s.ids.NewID(),
		Version:   1,
		Owner:     p.Owner,
		Parent:    &parentRef,
		Actor:     domain.Actor{Kind: domain.ActorTool, ID: agentP.Subject},
		Blocks:    resultBlocks([]callResult{{call: provider.ToolCallPart{CallID: call.CallID}, result: result}}),
		CreatedAt: s.clock.Now(),
	}
	return s.hist.Append(ctx, p, conversationID, turn, "turn:"+turn.ID)
}

// selectTools resolves which tools a step exposes and the narrowed principal
// they run under. names empty means every tool the principal may use.
func (s *stepper) selectTools(p kernel.Principal, names []string) (kernel.Principal, []Tool) {
	allowed := s.registry.Specs(p)
	if len(names) > 0 {
		allowed = filterSpecs(allowed, names)
	}

	// The narrowed scopes are the user's scopes intersected with what the
	// exposed tools ask for, so a tool can never widen the identity it runs
	// under.
	required := map[string]bool{}
	for _, spec := range allowed {
		if t, ok := s.registry.Lookup(spec.Name); ok {
			for _, scope := range t.Spec().Scopes {
				required[scope] = true
			}
		}
	}
	scopes := make([]string, 0, len(required))
	for _, scope := range p.Scopes {
		if required[scope] {
			scopes = append(scopes, scope)
		}
	}
	agentP := kernel.Principal{
		Owner:   p.Owner,
		Subject: p.Subject,
		Kind:    kernel.AgentPrincipal,
		Scopes:  scopes,
	}

	tools := make([]Tool, 0, len(allowed))
	for _, spec := range allowed {
		if t, ok := s.registry.Lookup(spec.Name); ok {
			tools = append(tools, t)
		}
	}
	return agentP, tools
}

// callResult is one executed call, kept paired with the call it answers so
// the committed block can name it.
type callResult struct {
	call   provider.ToolCallPart
	result ToolResult
}

// runTools executes the calls, up to maxParallel at a time, and returns
// results ordered by call index.
func (s *stepper) runTools(ctx context.Context, p kernel.Principal, calls []provider.ToolCallPart, maxParallel int) []callResult {
	results := make([]callResult, len(calls))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = callResult{call: call, result: s.runOne(ctx, p, call)}
		}()
	}
	wg.Wait()
	return results
}

// runOne executes a single call. A failure the model can see is an error
// result rather than a crashed step, so the model can adapt; a cancelled
// context aborts the whole step instead.
func (s *stepper) runOne(ctx context.Context, p kernel.Principal, call provider.ToolCallPart) ToolResult {
	t, ok := s.registry.Lookup(call.Name)
	if !ok {
		return errorResult("unknown tool: " + call.Name)
	}
	if len(call.Arguments) > 0 {
		if err := content.DecodeJSONValue(t.Spec().Schema, call.Arguments); err != nil {
			return errorResult("arguments do not match the tool's schema: " + err.Error())
		}
	}
	spec := t.Spec()
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}
	}
	res, err := t.Call(ctx, p, ToolCall{
		CallID: call.CallID, Name: call.Name, Arguments: call.Arguments,
	})
	if err != nil {
		return errorResult(err.Error())
	}
	return res
}

// splitByRisk separates the calls that may run now from the ones that pause
// for a person's approval. A RiskConfirm tool pauses the run; approving
// resumes it, and rejecting becomes an error result so the model can adapt.
// An unknown tool is left to run, where it becomes an error result — the
// model named something that does not exist and should be told so.
func splitByRisk(calls []provider.ToolCallPart, r Registry) (toRun []provider.ToolCallPart, pending []ToolCall) {
	for _, call := range calls {
		t, ok := r.Lookup(call.Name)
		if ok && t.Spec().Risk == RiskConfirm {
			pending = append(pending, ToolCall{
				CallID: call.CallID, Name: call.Name, Arguments: call.Arguments,
			})
			continue
		}
		toRun = append(toRun, call)
	}
	return toRun, pending
}

func (s *stepper) usage(model string, u provider.Usage) domain.Usage {
	return domain.Usage{
		Model:        model,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		CostMicros:   s.cost(model, u),
	}
}

func (s *stepper) cost(model string, u provider.Usage) int64 {
	if s.pricer == nil {
		return 0
	}
	return s.pricer(model, u)
}

// dispatchEffects runs each committed block's declared commands, in block
// order, under the step's principal. The idempotency key binds one dispatch
// to one block of one committed turn version, so a retried step converges
// on the stored receipt instead of repeating the mutation. Dispatch effects
// only when a registry is wired: without one no block type is known and no
// commands exist.
func (s *stepper) dispatchEffects(ctx context.Context, p kernel.Principal, turn domain.Turn) error {
	if s.content == nil || s.effects == nil {
		return nil
	}
	for i, blk := range turn.Blocks {
		eff, ok := s.content.EffectsFor(blk.Type)
		if !ok {
			continue
		}
		cmds, err := eff.Effects(ctx, blk)
		if err != nil {
			return kernel.WrapError(kernel.CodeInvalid, fmt.Sprintf("block %q at %d: effects could not be built", blk.Type, i), err)
		}
		key := fmt.Sprintf("turn:%s.%d:%d", turn.ID, turn.Version, i)
		for j, cmd := range cmds {
			if err := s.effects.DispatchEffect(ctx, p, cmd, key+":"+strconv.Itoa(j)); err != nil {
				return kernel.WrapError(kernel.CodeInternal, fmt.Sprintf("block %q at %d: effect dispatch failed", blk.Type, i), err)
			}
		}
	}
	return nil
}

// parseBlocks converts a response's parts into durable blocks through the
// block registry: text is scanned for workspace fences, thinking and native
// tool calls become typed blocks, and anything uninterpretable is kept as
// an unknown block with a diagnostic (never dropped). Without a registry
// the legacy static mapping applies.
func (s *stepper) parseBlocks(parts []provider.Part, nativeTools bool) ([]domain.Block, []content.Diagnostic) {
	if s.content == nil {
		return assistantBlocks(parts), nil
	}
	blocks, diags, err := content.ParseParts(s.content, parts, content.ParseOptions{
		Provider:    s.provider.Name(),
		NativeTools: nativeTools,
	})
	if err != nil {
		// Parsing itself failed; fall back to the static mapping so the
		// turn is never lost, and record the failure.
		return assistantBlocks(parts), []content.Diagnostic{{Code: "parse_failed"}}
	}
	return blocks, diags
}

// assistantBlocks maps the response's parts onto durable blocks. A thinking
// trace is display material and is not committed here; unknown parts are
// dropped rather than stored in a shape the pipeline cannot map back.
func assistantBlocks(parts []provider.Part) []domain.Block {
	blocks := make([]domain.Block, 0, len(parts))
	for _, part := range parts {
		switch v := part.(type) {
		case provider.TextPart:
			blocks = append(blocks, domain.Block{Type: "text", Value: map[string]any{"text": v.Text}})
		case provider.ToolCallPart:
			blocks = append(blocks, domain.Block{Type: "tool_call", Value: map[string]any{
				"call_id":   v.CallID,
				"name":      v.Name,
				"arguments": json.RawMessage(v.Arguments),
			}, Raw: json.RawMessage(v.Arguments)})
		}
	}
	return blocks
}

// toolCallParts pulls the tool calls out of a response in order.
func toolCallParts(parts []provider.Part) []provider.ToolCallPart {
	var calls []provider.ToolCallPart
	for _, part := range parts {
		if call, ok := part.(provider.ToolCallPart); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

// resultBlocks maps tool results onto blocks in call order, each naming the
// call it answers so the model can pair them.
func resultBlocks(results []callResult) []domain.Block {
	blocks := make([]domain.Block, 0, len(results))
	for _, res := range results {
		blocks = append(blocks, domain.Block{Type: "tool_result", Value: map[string]any{
			"call_id":  res.call.CallID,
			"output":   res.result.Output,
			"is_error": res.result.IsError,
		}})
	}
	return blocks
}

// errorResult renders a failure the model should see as an error result
// rather than as a crashed step.
func errorResult(message string) ToolResult {
	return ToolResult{Output: json.RawMessage(`{"error":` + strconv.Quote(message) + `}`), IsError: true}
}

func filterSpecs(specs []provider.ToolSpec, names []string) []provider.ToolSpec {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := make([]provider.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if want[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}
