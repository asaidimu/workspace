package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/asaidimu/workspace/domain"
)

func TestDebugCorrupt(t *testing.T) {
	f := newManagerFixture(t)
	if _, err := f.repos(t).Runs().Create(context.Background(), domain.Run{
		ID: "broken", ConversationID: "c1", Status: domain.RunRunning,
		State: json.RawMessage("{not json"),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.clock.Advance(time.Hour)
	if err := f.manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	got := f.run(t, "broken")
	t.Logf("status=%q failure=%q worker=%q hb=%v", got.Status, got.Failure, got.Worker, got.HeartbeatAt)
	t.Logf("events=%v", f.emitter.names())
	_ = domain.RunRunning
}
