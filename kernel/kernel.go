// Package kernel turns every change into a command that runs authorized,
// ordered and atomic, and publishes events only after the commit succeeds.
//
// It is generic and imports only the store interfaces, so it runs on any
// store. A command passes through seven steps (Workspace specification,
// section 7): build an envelope, run the middleware chain, authorize, admit
// through the keyed serializer, run store.Atomic (reserving the idempotency
// key first when one is given), hold the handler's events until commit, then
// publish them — or run compensation hooks after rollback.
package kernel

import (
	"context"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// Clock and IDGenerator are re-exported from domain so the kernel's callers
// need only one import.
type Clock = domain.Clock
type IDGenerator = domain.IDGenerator

// Command is a unit of change. Its concrete type is the dispatch key.
type Command interface {
	CommandName() string
}

// Event is something that happened inside a command. Events reach the bus
// only after the commit is durable.
type Event interface {
	EventName() string
}

// PrincipalKind distinguishes the three sorts of actor.
type PrincipalKind int

const (
	UserPrincipal PrincipalKind = iota
	AgentPrincipal
	SystemPrincipal
)

// Principal is who is acting. Owner scopes every read and write; Scopes are
// strings like "workspace:turn:append" that the authorizer checks.
type Principal struct {
	Owner   string
	Subject string
	Kind    PrincipalKind
	Scopes  []string
}

// Envelope is a command with its tracing and idempotency metadata.
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

// Target names the resource a command acts on. Kind is "conversation",
// "content", "role" or "run"; it is empty for owner-level commands.
type Target struct {
	Kind string
	ID   string
}

// Authorizer decides whether a principal may run a command. The default is
// deny; opening a workspace without one is an error. AllowOwner() is provided
// for single-user apps.
type Authorizer interface {
	Authorize(ctx context.Context, p Principal, commandName string, t Target) error
}

// Handler is the body of a command: it runs inside store.Atomic and returns
// the result plus the events to publish after commit.
type Handler[C Command, R any] func(ctx context.Context, tx store.Tx, c C) (R, []Event, error)

// Next is the tail of the middleware chain.
type Next func(ctx context.Context, env Envelope) (any, error)

// Middleware wraps the dispatch path. The chain runs outermost first:
// panic recovery, tracing, logging, validation.
type Middleware func(next Next) Next

// EventEnvelope is a published event with its delivery metadata.
type EventEnvelope struct {
	Name           string
	Owner          string
	ConversationID string // empty for owner-level events
	CorrelationID  string
	At             time.Time
	Event          Event
}

// EventFilter selects which events a subscriber receives.
type EventFilter struct {
	Owner          string
	ConversationID string   // empty matches all
	Names          []string // empty matches all
}

// Bus delivers committed events in-process, ordered per conversation and at
// most once. A subscriber that cannot keep up is disconnected after a final
// overflow event and must re-sync by querying.
type Bus interface {
	Subscribe(ctx context.Context, f EventFilter, buffer int) (<-chan EventEnvelope, error)
}
