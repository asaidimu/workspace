// Package agent is the run manager and the tool registry.
//
// A restart repeats at most the step in flight: one model call, or the tool
// calls that had no committed result. The assistant turn, tool calls
// included, is committed before any tool runs, and results are committed
// before the next step. The built-in loop is a small policy over a public
// step primitive; a custom loop calls Step itself and still gets durable
// history, authorization, validation, events and cost accounting, because
// those belong to the step and not to the loop.
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/asaidimu/workspace/content"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
)

// Risk classifies a tool for approval purposes.
type Risk int

const (
	// RiskLow runs without pausing.
	RiskLow Risk = iota
	// RiskConfirm pauses the run until a person approves.
	RiskConfirm
)

// ToolSpec advertises a tool to the registry and, through the prompt
// pipeline, to the model. Scopes are the scopes the calling principal must
// hold; a tool can never widen the narrowed agent identity it runs under.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Scopes      []string
	Risk        Risk
	Idempotent  bool
	Timeout     time.Duration // zero selects the run default
	MaxOutput   int           // bytes; larger results are truncated and flagged
}

// ToolCall is one requested invocation.
type ToolCall struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is what a tool returned. IsError marks a failure the model
// should see as an error result rather than a crash.
type ToolResult struct {
	Output  json.RawMessage
	IsError bool
}

// Tool is a callable function plus its spec.
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, p kernel.Principal, call ToolCall) (ToolResult, error)
}

// Registry is the tool table. Specs filters by what the principal may use.
type Registry interface {
	Register(t Tool) error
	Lookup(name string) (Tool, bool)
	Specs(p kernel.Principal) []provider.ToolSpec
}

// RunOptions bounds one run: steps (25), deadline (10 minutes) and a cost
// cap in micro-KES. Exceeding any one fails the run with a specific code,
// and all committed work stays in the history.
type RunOptions struct {
	Model         string
	MaxSteps      int
	MaxParallel   int
	Deadline      time.Duration
	CostCapMicros int64
	Instructions  string
	// Tools narrows a run's exposed tools to these names; empty means every
	// tool the principal may use (run.start carries the list).
	Tools []string
}

// Manager owns runs. Each run is one goroutine; each provider stream has its
// own.
type Manager interface {
	Start(ctx context.Context, p kernel.Principal, conversationID string, opts RunOptions) (domain.Run, error)
	Cancel(ctx context.Context, p kernel.Principal, runID string) error
	Approve(ctx context.Context, p kernel.Principal, runID, callID string, approve bool, reason string) error
	Recover(ctx context.Context) error // claims stale runs and resumes them
}

// StepOptions bounds one step of a custom loop.
type StepOptions struct {
	Model        string
	MaxParallel  int
	Instructions string
	Tools        []string // tool names to expose; empty means every tool the principal may use
	// Constraints are this call's opinion on the request knobs. They merge
	// over the conversation's and the role's, so a caller can tighten a
	// single step without writing anything durable.
	Constraints *domain.Constraints
}

// StepResult is what one step committed.
type StepResult struct {
	Turns       []domain.Turn // the assistant turn and, when tools ran, the tool-result turn
	Finish      provider.FinishReason
	Pending     []ToolCall // calls waiting for approval
	Usage       provider.Usage
	CostMicros  int64                // in micro-KES
	Diagnostics []content.Diagnostic // problems found parsing the model's output; never fatal
}

// EffectsDispatcher runs the commands a committed block declares, under a
// principal with an idempotency key naming the block, so a replay changes
// nothing. It is a port because only a host knows how commands map onto
// its kernel: the agent names the contract and the failure rule (an error
// fails the step; the assistant turn stays committed and a retry repeats
// the dispatch, converging on its receipt), never the dispatch site.
type EffectsDispatcher interface {
	DispatchEffect(ctx context.Context, p kernel.Principal, cmd kernel.Command, idempotencyKey string) error
}

// Stepper is the public step primitive a custom loop drives itself.
type Stepper interface {
	Step(ctx context.Context, p kernel.Principal, conversationID string, opts StepOptions) (StepResult, error)
}

// Runner is the stepper plus the approval resolver: one constructed object
// drives a step and finishes a parked call, so a host wires a single value
// into both the manager's Stepper and Resolver slots.
type Runner interface {
	Stepper
	CallResolver
}

// Checkpointer persists a loop's own state in the run's checkpoint, so a
// loop that wants crash recovery keeps its state there.
type Checkpointer interface {
	Checkpoint(ctx context.Context, p kernel.Principal, runID string, expectedStep int, state json.RawMessage) (domain.Run, error)
}
