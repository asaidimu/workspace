package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/prompt"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/memory"
	"github.com/asaidimu/workspace/workspacetest"
)

// TestCancelRunClosesTheProviderRequestWithinOneSecond is the section 3
// target's integration half: a run parked inside a slow provider call is
// cancelled through the manager, and both the provider request and the run
// settle within one second. The replay provider's slow-generate transcript
// stands in for a network call.
func TestCancelRunClosesTheProviderRequestWithinOneSecond(t *testing.T) {
	prov := loadReplay(t, "slow-generate")

	line := &timeline{}
	hist := newHistory(line)
	clock := workspacetest.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ids := workspacetest.NewSeqIDs("id")

	stepper, err := agent.NewStepper(agent.StepperConfig{
		History:  hist,
		Pipeline: prompt.DefaultPipeline(),
		Provider: prov,
		Registry: agent.NewRegistry(),
		Pricer: func(model string, u provider.Usage) int64 {
			return int64(u.InputTokens+u.OutputTokens) * 10
		},
		Clock: clock,
		IDs:   ids,
	})
	if err != nil {
		t.Fatalf("NewStepper: %v", err)
	}
	resolver, ok := stepper.(agent.CallResolver)
	if !ok {
		t.Fatal("the step primitive must resolve pending calls")
	}

	st := memory.New(memory.Options{Clock: clock})
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	emitter := &recordingEmitter{}
	mgr, err := agent.NewManager(agent.ManagerConfig{
		Store:    st,
		Stepper:  stepper,
		Resolver: resolver,
		Emitter:  emitter,
		Clock:    clock,
		IDs:      ids,
		Worker:   "test-worker",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	repos, err := st.Scoped(store.Scope{Owner: "u1"})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}

	// SLOW_GENERATE appears in the assembled system prompt, which is what
	// the transcript's contains match keys on.
	run, err := mgr.Start(context.Background(), testPrincipal(), "c1", agent.RunOptions{
		Model:        "replay-model",
		Instructions: "SLOW_GENERATE",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitStatus(t, "the provider call to be in flight", func() bool { return prov.Open() == 1 })

	start := time.Now()
	if err := mgr.Cancel(context.Background(), testPrincipal(), run.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("Cancel returned after %v, want within 1s", elapsed)
	}

	waitStatus(t, "the provider request to close", func() bool { return prov.Open() == 0 })
	got, err := repos.Runs().Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if got.Status != domain.RunCancelled {
		t.Errorf("run status = %v, want cancelled", got.Status)
	}
	if emitter.has("run.cancelled") {
		// Cancel announces the cancellation itself; the loop must not
		// announce it a second time.
		n := 0
		for _, name := range emitter.names() {
			if name == "run.cancelled" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("run.cancelled announced %d times, want 1", n)
		}
	}
}

// loadReplay picks one transcript out of testdata/replays so a test's
// provider answers exactly one shape of request.
func loadReplay(t *testing.T, name string) *workspacetest.ReplayProvider {
	t.Helper()
	for _, tr := range workspacetest.LoadReplayTranscripts(t, "../testdata/replays") {
		if tr.Name == name {
			return workspacetest.NewReplayProvider("replay", tr)
		}
	}
	t.Fatalf("no replay transcript named %q", name)
	return nil
}
