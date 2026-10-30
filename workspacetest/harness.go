package workspacetest

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/asaidimu/workspace"
	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/blob"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// harnessConfig is the mutable options bag HarnessOption writes into.
type harnessConfig struct {
	owner     string
	startTime time.Time
	tools     []agent.Tool
}

// HarnessOption configures one harness.
type HarnessOption func(*harnessConfig)

// WithOwner sets the owner the default handle runs as (default "test").
func WithOwner(owner string) HarnessOption {
	return func(c *harnessConfig) { c.owner = owner }
}

// WithStartTime sets the fake clock's initial instant.
func WithStartTime(t time.Time) HarnessOption {
	return func(c *harnessConfig) { c.startTime = t }
}

// WithTools registers tools on the workspace the harness opens, for tests
// that drive a tool-calling script.
func WithTools(tools ...agent.Tool) HarnessOption {
	return func(c *harnessConfig) { c.tools = tools }
}

// Harness is a real workspace wired to doubles: a fake clock, a scripted
// provider and an event log. Tests drive it through Handle methods and wait
// on Events rather than sleeping.
type Harness struct {
	WS       *workspace.Workspace
	Store    store.Store
	Blobs    blob.Store
	Clock    *FakeClock
	Provider *ScriptedProvider
	Events   *EventLog
	owner    string
	ids      *SeqIDs
	tools    []agent.Tool
	unsub    context.CancelFunc
}

// NewHarness opens a workspace on the given store and blob store with a
// scripted provider, a fake clock starting at a fixed instant, and an event
// log subscribed to the workspace bus.
//
// The store and blobs are parameters rather than construction details so a
// test can hand in the memory store, the anansistore on SQLite, or a faulty
// wrapper for failure injection — the same harness serves all three.
func NewHarness(t testing.TB, st store.Store, bs blob.Store, opts ...HarnessOption) *Harness {
	t.Helper()
	cfg := harnessConfig{
		owner:     "test",
		startTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, o := range opts {
		o(&cfg)
	}

	h := &Harness{
		Store:    st,
		Blobs:    bs,
		Clock:    NewFakeClock(cfg.startTime),
		Provider: NewScriptedProvider("scripted"),
		Events:   NewEventLog(),
		owner:    cfg.owner,
		tools:    cfg.tools,
		// The ID generator is the harness's, not the workspace's, so a
		// Restart reopens on the same store without minting an identifier
		// the data already uses.
		ids: NewSeqIDs("id"),
	}
	h.open(t)
	t.Cleanup(func() {
		h.abandon()
	})
	return h
}

// open builds a workspace over the harness's doubles and subscribes the
// event log to its bus. Restart reuses it, so everything a second open must
// share with the first lives on the harness rather than in a closure.
func (h *Harness) open(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ws, err := workspace.Open(ctx, workspace.Config{
		Store: h.Store,
		Blobs: h.Blobs,
		// A discard logger keeps a passing test's command log out of the
		// output while a failing one still reports through t.
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Providers:  []provider.Provider{h.Provider},
		Authorizer: kernel.AllowOwner(),
		Clock:      h.Clock,
		IDs:        h.ids,
		Tools:      h.tools,
	})
	if err != nil {
		cancel()
		t.Fatalf("workspace.Open: %v", err)
	}
	h.WS = ws
	h.unsub = cancel

	sub, err := ws.Events().Subscribe(ctx, kernel.EventFilter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	go func() {
		for env := range sub {
			h.Events.Record(env)
		}
	}()
}

// abandon tears the current workspace down without closing it, which is what
// a crash-recovery test means by "the workspace went away": the store keeps
// everything, and the next open starts from the store alone.
func (h *Harness) abandon() {
	if h.unsub != nil {
		h.unsub()
		h.unsub = nil
	}
	h.WS = nil
}

// As returns a handle scoped to the given owner.
func (h *Harness) As(owner string) *workspace.Handle {
	if h.WS == nil {
		return nil
	}
	return h.WS.For(kernel.Principal{Owner: owner, Subject: owner, Kind: kernel.UserPrincipal})
}

// Restart abandons the workspace without Close and reopens it on the same
// store, which is what a crash-recovery test needs.
func (h *Harness) Restart(t testing.TB) {
	t.Helper()
	h.abandon()
	h.open(t)
}

// compile-time assertion that the facade and the doubles still line up.
var _ = provider.Capabilities{}
