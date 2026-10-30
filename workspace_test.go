package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asaidimu/workspace"
	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/blob"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
	"github.com/asaidimu/workspace/store/memory"
	"github.com/asaidimu/workspace/workspacetest"
)

// newHarness opens the milestone setup: the in-memory store, an in-memory
// blob store, a scripted provider, a fake clock and an event log, with the
// workspace wired through workspace.Open.
func newHarness(t *testing.T, opts ...workspacetest.HarnessOption) *workspacetest.Harness {
	t.Helper()
	return workspacetest.NewHarness(t,
		memory.New(memory.Options{}),
		workspacetest.NewMemoryBlobs(),
		opts...)
}

// waitRun spins on the run until it reports a terminal state, yielding to
// the loop's goroutine rather than sleeping: section 16 forbids sleeps, and
// a bounded wall-clock deadline turns a hung run into a failure instead of a
// hung test.
func waitRun(t *testing.T, handle *workspace.Handle, runID string) domain.Run {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	var last domain.Run
	for {
		run, err := handle.Runs().Get(ctx, runID)
		lastErr, last = err, run
		if err == nil && run.Status.IsTerminal() {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never finished: status=%q err=%v", runID, last.Status, lastErr)
		}
		runtime.Gosched()
	}
}

// TestBlobResolverMapsBlobsToProviderParts covers the workspace's resolver:
// an uploaded blob referenced from a block resolves to a media part the
// adapter streams from, the owner scoping hides other owners' blobs, and a
// provider-side remote id takes precedence when one is recorded.
func TestBlobResolverMapsBlobsToProviderParts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")

	info, err := handle.Blobs().Upload(ctx, strings.NewReader("fake png bytes"),
		workspace.UploadOptions{MediaType: "image/png", Name: "pic.png"})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	resolver := h.WS.BlobResolver()
	part, err := resolver.Resolve(ctx, info.Owner, info.ID, "openai")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	img, ok := part.(provider.ImagePart)
	if !ok {
		t.Fatalf("part = %#v, want an ImagePart", part)
	}
	if img.Source == nil {
		t.Fatal("the part must carry a source before any upload happened")
	}
	if img.Source.MediaType() != "image/png" {
		t.Errorf("media type = %q, want image/png", img.Source.MediaType())
	}
	rc, err := img.Source.Open(ctx)
	if err != nil {
		t.Fatalf("Source.Open: %v", err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("Source read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(body) != "fake png bytes" {
		t.Errorf("bytes = %q, want the uploaded payload", body)
	}

	if _, err := resolver.Resolve(ctx, "other", info.ID, "openai"); err == nil {
		t.Error("another owner's resolver must not see this blob")
	}

	// A recorded remote wins over the byte stream: the adapter sends its
	// own file id and never opens the bytes again.
	repos, err := h.Store.Scoped(store.Scope{Owner: info.Owner})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	if err := repos.Blobs().SetRemote(ctx, info.ID, "openai", domain.RemoteRef{ID: "file-openai-1"}); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	part, err = resolver.Resolve(ctx, info.Owner, info.ID, "openai")
	if err != nil {
		t.Fatalf("Resolve with remote: %v", err)
	}
	img, ok = part.(provider.ImagePart)
	if !ok || img.RemoteID != "file-openai-1" || img.Source != nil {
		t.Fatalf("part = %#v, want the remote-id ImagePart", part)
	}
}

// TestSettingsUpdateAndGet covers the settings.get/settings.update command
// family: the first write creates the row at revision 1, the next bumps it,
// a stale revision conflicts, the update event is published, and a
// principal without an owner is refused.
func TestSettingsUpdateAndGet(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")

	if _, err := handle.Settings().Get(ctx); !kernel.IsCode(err, kernel.CodeNotFound) {
		t.Fatalf("Settings.Get on a fresh workspace = %v, want NOT_FOUND", err)
	}

	prompt := "Be precise."
	lang := "en"
	saved, err := handle.Settings().Update(ctx, workspace.UpdateSettings{
		Prompt: &prompt, Language: &lang, ExpectedRev: 0,
	})
	if err != nil {
		t.Fatalf("Settings.Update: %v", err)
	}
	if saved.Rev != 1 || saved.Prompt != prompt || saved.Language != lang {
		t.Fatalf("settings = %+v, want revision 1 with the patch applied", saved)
	}
	if _, err := h.Events.WaitFor(withTimeout(t), "settings.updated"); err != nil {
		t.Fatalf("settings.updated event: %v", err)
	}

	got, err := handle.Settings().Get(ctx)
	if err != nil || got.Prompt != prompt || got.Rev != 1 {
		t.Fatalf("Settings.Get = %+v, %v; want the saved row", got, err)
	}

	// A nil field leaves its setting alone; the revision bumps.
	lang2 := "sw"
	saved, err = handle.Settings().Update(ctx, workspace.UpdateSettings{Language: &lang2, ExpectedRev: 1})
	if err != nil {
		t.Fatalf("second Update: %v", err)
	}
	if saved.Rev != 2 || saved.Prompt != prompt || saved.Language != lang2 {
		t.Fatalf("settings = %+v, want revision 2 keeping the prompt", saved)
	}

	if _, err := handle.Settings().Update(ctx, workspace.UpdateSettings{Prompt: &prompt, ExpectedRev: 1}); !kernel.IsCode(err, kernel.CodeConflict) {
		t.Fatalf("stale revision = %v, want CONFLICT", err)
	}

	// Authorization: the default allow-owner authorizer refuses a principal
	// with no owner before the handler runs.
	if _, err := h.As("").Settings().Update(ctx, workspace.UpdateSettings{Prompt: &prompt}); !kernel.IsCode(err, kernel.CodeForbidden) {
		t.Fatalf("ownerless update = %v, want FORBIDDEN", err)
	}
}

// TestConversationPreferencesAndConstraints covers the conversation fields
// the prompt pipeline reads: preference keys survive create and update, and
// constraints round-trip as stored JSON.
func TestConversationPreferencesAndConstraints(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")

	prefs := []domain.ContentRef{{Key: "style", Version: 1}}
	raw := json.RawMessage(`{"temperature":0.3}`)
	conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{
		Title: "tuned", PreferenceKeys: prefs, Constraints: raw,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(conv.PreferenceKeys) != 1 || conv.PreferenceKeys[0].Key != "style" {
		t.Fatalf("preference keys = %+v, want the created ref", conv.PreferenceKeys)
	}
	if string(conv.Constraints) != string(raw) {
		t.Fatalf("constraints = %s, want %s", conv.Constraints, raw)
	}

	more := []domain.ContentRef{{Key: "tone", Version: 2}}
	raw2 := json.RawMessage(`{"temperature":0.7,"stop":["\n\n"]}`)
	conv, err = handle.Conversations().Update(ctx, conv.ID, workspace.UpdateConversation{
		PreferenceKeys: &more, Constraints: &raw2,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(conv.PreferenceKeys) != 1 || conv.PreferenceKeys[0].Key != "tone" {
		t.Fatalf("preference keys = %+v, want the replacement", conv.PreferenceKeys)
	}
	if string(conv.Constraints) != string(raw2) {
		t.Fatalf("constraints = %s, want %s", conv.Constraints, raw2)
	}
}

// startRun creates a conversation with one user turn and starts a run over
// it, returning the handle, the conversation and the run.
func startRun(t *testing.T, h *workspacetest.Harness, message string) (*workspace.Handle, domain.Conversation, domain.Run) {
	t.Helper()
	ctx := context.Background()
	handle := h.As("test")
	conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{Title: "milestone"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := handle.Turns().Append(ctx, conv.ID, workspace.AppendTurn{
		Actor:  domain.Actor{Kind: domain.ActorUser, ID: "test"},
		Blocks: workspace.Text(message),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	run, err := handle.Runs().Start(ctx, conv.ID, workspace.StartRun{Model: "scripted"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return handle, conv, run
}

// chainOf reads a conversation's active chain, oldest first.
func chainOf(t *testing.T, handle *workspace.Handle, conversationID string) []domain.Turn {
	t.Helper()
	chain, err := handle.Turns().Chain(context.Background(), conversationID, store.ChainOptions{})
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	return chain
}

// asObject renders a block value — a typed payload struct or a plain map —
// as the generic object tests assert on. The registry's Decode binds typed
// payloads, so both shapes are legitimate.
func asObject(t *testing.T, turn domain.Turn, blockType string, value any) map[string]any {
	t.Helper()
	if m, ok := value.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("turn %s %s: value does not marshal: %v", turn.ID, blockType, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("turn %s %s value is %T, want a map-shaped payload", turn.ID, blockType, value)
	}
	return m
}

// textOf returns the string in a turn's text block.
func textOf(t *testing.T, turn domain.Turn) string {
	t.Helper()
	for _, blk := range turn.Blocks {
		if blk.Type != "text" {
			continue
		}
		m := asObject(t, turn, "text", blk.Value)
		s, ok := m["text"].(string)
		if !ok {
			t.Fatalf("turn %s text has no string field", turn.ID)
		}
		return s
	}
	t.Fatalf("turn %s has no text block", turn.ID)
	return ""
}

// blockValue returns one named block's payload as a map.
func blockValue(t *testing.T, turn domain.Turn, blockType string) map[string]any {
	t.Helper()
	for _, blk := range turn.Blocks {
		if blk.Type != blockType {
			continue
		}
		return asObject(t, turn, blockType, blk.Value)
	}
	t.Fatalf("turn %s has no %s block", turn.ID, blockType)
	return nil
}

// TestFullConversationInProcess is the milestone: a complete conversation —
// create, user turn, agent run, committed reply — over store/memory and the
// scripted provider, with no network, no real clock and no sleeping.
func TestFullConversationInProcess(t *testing.T) {
	h := newHarness(t)
	h.Provider.ReplyText("hello from the model")

	handle, conv, run := startRun(t, h, "hello workspace")

	final := waitRun(t, handle, run.ID)
	if final.Status != domain.RunSucceeded {
		t.Fatalf("run finished %s (failure %q), want succeeded", final.Status, final.Failure)
	}

	chain := chainOf(t, handle, conv.ID)
	if len(chain) != 2 {
		t.Fatalf("chain has %d turns, want user + assistant: %s", len(chain), describe(chain))
	}
	if chain[0].Actor.Kind != domain.ActorUser {
		t.Errorf("first turn actor = %s, want user", chain[0].Actor.Kind)
	}
	if chain[1].Actor.Kind != domain.ActorAgent {
		t.Errorf("second turn actor = %s, want agent", chain[1].Actor.Kind)
	}
	if got := textOf(t, chain[1]); got != "hello from the model" {
		t.Errorf("assistant text = %q, want %q", got, "hello from the model")
	}

	// The prompt the run built carried the user's message.
	reqs := h.Provider.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider was called %d times, want 1", len(reqs))
	}
	if !requestContains(reqs[0], "hello workspace") {
		t.Errorf("provider request does not carry the user text: %s", describeRequest(reqs[0]))
	}

	// Section 13's events, recorded by the harness's bus subscription.
	for _, name := range []string{"conversation.created", "turn.appended", "run.started"} {
		if _, err := h.Events.WaitFor(withTimeout(t), name); err != nil {
			t.Errorf("event %s: %v", name, err)
		}
	}
}

// TestRunWithAToolRoundTrip drives the order section 11 pins: the assistant
// turn is committed before the tool runs, the tool's result before the next
// step, and the final answer lands after both.
func TestRunWithAToolRoundTrip(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	echo := agent.NewTool(agent.ToolSpec{Name: "echo"}, func(ctx context.Context, p kernel.Principal, in struct {
		Msg string `json:"msg"`
	}) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "echo:" + in.Msg, nil
	})

	h := newHarness(t, workspacetest.WithTools(echo))
	h.Provider.ReplyToolCalls(provider.ToolCallPart{
		CallID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"msg":"ping"}`),
	})
	h.Provider.ReplyText("all done")

	handle, conv, run := startRun(t, h, "use the tool")

	final := waitRun(t, handle, run.ID)
	if final.Status != domain.RunSucceeded {
		t.Fatalf("run finished %s (failure %q), want succeeded", final.Status, final.Failure)
	}

	mu.Lock()
	ran := calls
	mu.Unlock()
	if ran != 1 {
		t.Fatalf("tool ran %d times, want exactly 1", ran)
	}

	chain := chainOf(t, handle, conv.ID)
	if len(chain) != 4 {
		t.Fatalf("chain has %d turns, want user, call, result, final: %s", len(chain), describe(chain))
	}
	if chain[0].Actor.Kind != domain.ActorUser {
		t.Errorf("chain[0] actor = %s, want user", chain[0].Actor.Kind)
	}
	if chain[1].Actor.Kind != domain.ActorAgent {
		t.Errorf("chain[1] actor = %s, want agent (the committed call)", chain[1].Actor.Kind)
	}
	if call := blockValue(t, chain[1], "tool_call"); call["name"] != "echo" {
		t.Errorf("call names %v, want echo", call["name"])
	}
	if chain[2].Actor.Kind != domain.ActorTool {
		t.Errorf("chain[2] actor = %s, want tool", chain[2].Actor.Kind)
	}
	result := blockValue(t, chain[2], "tool_result")
	if !strings.Contains(fmt.Sprint(result["output"]), "echo:ping") {
		t.Errorf("tool result output = %v, want it to carry echo:ping", result["output"])
	}
	if chain[3].Actor.Kind != domain.ActorAgent || textOf(t, chain[3]) != "all done" {
		t.Errorf("final turn = %s/%q, want agent/all done", chain[3].Actor.Kind, textOf(t, chain[3]))
	}

	// The second step saw the call and its result in the transcript.
	reqs := h.Provider.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider was called %d times, want 2 (call, then final)", len(reqs))
	}
	if !requestContains(reqs[1], "echo:ping") {
		t.Errorf("second request does not carry the tool result: %s", describeRequest(reqs[1]))
	}
}

// waitStatus spins until a run reaches a non-terminal status it is waiting
// for, failing if the run ends first.
func waitStatus(t *testing.T, handle *workspace.Handle, runID string, want domain.RunStatus) domain.Run {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := handle.Runs().Get(ctx, runID)
		if err != nil {
			t.Fatalf("Runs.Get: %v", err)
		}
		if run.Status == want {
			return run
		}
		if run.Status.IsTerminal() {
			t.Fatalf("run reached %s (failure %q) before %s", run.Status, run.Failure, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %s, want %s", run.Status, want)
		}
		runtime.Gosched()
	}
}

// TestApprovalResumesAParkedRun walks section 11's approval flow through the
// facade: a RiskConfirm tool parks the run without executing, and approving
// runs it and lets the loop finish.
func TestApprovalResumesAParkedRun(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	calls := 0
	confirmer := agent.NewTool(agent.ToolSpec{Name: "wipe", Risk: agent.RiskConfirm}, func(ctx context.Context, p kernel.Principal, in struct{}) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "wiped", nil
	})

	h := newHarness(t, workspacetest.WithTools(confirmer))
	h.Provider.ReplyToolCalls(provider.ToolCallPart{
		CallID: "call-9", Name: "wipe", Arguments: json.RawMessage(`{}`),
	})
	h.Provider.ReplyText("done after approval")

	handle, conv, run := startRun(t, h, "wipe it")

	waitStatus(t, handle, run.ID, domain.RunWaiting)
	mu.Lock()
	parkedCalls := calls
	mu.Unlock()
	if parkedCalls != 0 {
		t.Fatalf("a RiskConfirm tool ran before approval: %d calls", parkedCalls)
	}
	// The assistant's request is committed; its result is not.
	chain := chainOf(t, handle, conv.ID)
	if len(chain) != 2 {
		t.Fatalf("chain while parked = %s, want user + committed call", describe(chain))
	}
	blockValue(t, chain[1], "tool_call")
	if _, err := h.Events.WaitFor(withTimeout(t), "run.approval_requested"); err != nil {
		t.Errorf("run.approval_requested: %v", err)
	}

	if err := handle.Runs().Approve(ctx, run.ID, "call-9", true, "go ahead"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	final := waitRun(t, handle, run.ID)
	if final.Status != domain.RunSucceeded {
		t.Fatalf("run finished %s (failure %q), want succeeded", final.Status, final.Failure)
	}
	mu.Lock()
	total := calls
	mu.Unlock()
	if total != 1 {
		t.Fatalf("tool ran %d times after approval, want 1", total)
	}
	chain = chainOf(t, handle, conv.ID)
	if len(chain) != 4 {
		t.Fatalf("chain after approval = %s, want user, call, result, final", describe(chain))
	}
	if got := textOf(t, chain[3]); got != "done after approval" {
		t.Errorf("final text = %q, want it to come from the second script line", got)
	}
	if _, err := h.Events.WaitFor(withTimeout(t), "run.resumed"); err != nil {
		t.Errorf("run.resumed: %v", err)
	}
}

// TestEditBranchAndFork walks the faithful-history rules: an edit files a
// new version under the same parent, a branch switch moves only the head,
// and a fork writes one row into the shared tree.
func TestEditBranchAndFork(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")

	conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{Title: "branching"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := handle.Turns().Append(ctx, conv.ID, workspace.AppendTurn{
		Actor:  domain.Actor{Kind: domain.ActorUser, ID: "test"},
		Blocks: workspace.Text("v1"),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// An edit of the head turn files version 2 of the same ID, and the head
	// follows it: the conversation now reads the edited text.
	edited, err := handle.Turns().Edit(ctx, conv.ID, first.Ref(), workspace.EditTurn{
		Blocks: workspace.Text("v2"),
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if edited.ID != first.ID || edited.Version != 2 {
		t.Fatalf("edit produced %s v%d, want %s v2", edited.ID, edited.Version, first.ID)
	}
	chain := chainOf(t, handle, conv.ID)
	if len(chain) != 1 || chain[0].Version != 2 {
		t.Fatalf("chain after edit = %s, want exactly the edited version", describe(chain))
	}
	if got := textOf(t, chain[0]); got != "v2" {
		t.Errorf("edited text = %q, want v2", got)
	}

	// v1 still exists — nothing was rewritten — and branches reports both
	// versions with the head as the active one.
	branches, err := handle.Turns().Branches(ctx, conv.ID, first.Ref())
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(branches.Siblings) != 2 {
		t.Errorf("siblings = %d, want 2 (v1 and v2)", len(branches.Siblings))
	}
	if branches.Active != edited.Ref() {
		t.Errorf("active = %v, want the edited version %v", branches.Active, edited.Ref())
	}

	// Switching back moves only the pointer.
	moved, err := handle.Turns().Switch(ctx, conv.ID, first.Ref())
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if moved.Head == nil || *moved.Head != first.Ref() {
		t.Fatalf("head after switch = %v, want %v", moved.Head, first.Ref())
	}
	chain = chainOf(t, handle, conv.ID)
	if len(chain) != 1 || chain[0].Version != 1 {
		t.Fatalf("chain after switch = %s, want v1", describe(chain))
	}
	if got := textOf(t, chain[0]); got != "v1" {
		t.Errorf("switched text = %q, want the original v1", got)
	}

	// A fork writes one conversation row pointing at the chosen turn; the
	// turn tree itself is shared, not copied.
	fork, err := handle.Conversations().Fork(ctx, conv.ID, first.Ref(), "forked")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if fork.ID == conv.ID {
		t.Error("fork reused the source conversation's ID")
	}
	if fork.Head == nil || *fork.Head != first.Ref() {
		t.Errorf("fork head = %v, want %v", fork.Head, first.Ref())
	}
	if fork.ForkedFrom == nil || *fork.ForkedFrom != first.Ref() {
		t.Errorf("forked-from = %v, want %v", fork.ForkedFrom, first.Ref())
	}
	forkChain := chainOf(t, handle, fork.ID)
	if len(forkChain) != 1 || forkChain[0].ID != first.ID {
		t.Errorf("fork chain = %s, want the shared v1 turn", describe(forkChain))
	}
	// The fork does not disturb the source conversation, which still reads
	// its switched-back head.
	sourceChain := chainOf(t, handle, conv.ID)
	if len(sourceChain) != 1 || sourceChain[0].Version != 1 {
		t.Errorf("source chain after fork = %s, want its own v1", describe(sourceChain))
	}
}

// TestHandleErrorsCarryCodes pins the eight-code contract on the facade:
// callers branch on Code, never on message text.
func TestHandleErrorsCarryCodes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")

	t.Run("a principal without an owner is forbidden", func(t *testing.T) {
		bare := h.WS.For(kernel.Principal{})
		_, err := bare.Conversations().Get(ctx, "anything")
		if !kernel.IsCode(err, kernel.CodeForbidden) {
			t.Errorf("err = %v, want FORBIDDEN", err)
		}
	})

	t.Run("a missing conversation is not found", func(t *testing.T) {
		_, err := handle.Conversations().Get(ctx, "nope")
		if !kernel.IsCode(err, kernel.CodeNotFound) {
			t.Errorf("err = %v, want NOT_FOUND", err)
		}
	})

	t.Run("a conversation may not name a missing role", func(t *testing.T) {
		_, err := handle.Conversations().Create(ctx, workspace.CreateConversation{RoleID: "ghost"})
		if !kernel.IsCode(err, kernel.CodeInvalid) {
			t.Errorf("err = %v, want INVALID", err)
		}
	})

	t.Run("only user and system turns may be appended", func(t *testing.T) {
		conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{Title: "actors"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = handle.Turns().Append(ctx, conv.ID, workspace.AppendTurn{
			Actor:  domain.Actor{Kind: domain.ActorAgent, ID: "spoof"},
			Blocks: workspace.Text("pretending"),
		})
		if !kernel.IsCode(err, kernel.CodeInvalid) {
			t.Errorf("err = %v, want INVALID", err)
		}
	})

	t.Run("an empty turn is refused", func(t *testing.T) {
		conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{Title: "empty"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = handle.Turns().Append(ctx, conv.ID, workspace.AppendTurn{
			Actor: domain.Actor{Kind: domain.ActorUser, ID: "test"},
		})
		if !kernel.IsCode(err, kernel.CodeInvalid) {
			t.Errorf("err = %v, want INVALID", err)
		}
	})

	t.Run("a run needs a model", func(t *testing.T) {
		conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{Title: "runs"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := handle.Runs().Start(ctx, conv.ID, workspace.StartRun{}); !kernel.IsCode(err, kernel.CodeInvalid) {
			t.Errorf("err = %v, want INVALID", err)
		}
	})

	t.Run("editing a turn that does not exist is not found", func(t *testing.T) {
		conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{Title: "edits"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = handle.Turns().Edit(ctx, conv.ID, domain.TurnRef{ID: "ghost", Version: 1}, workspace.EditTurn{
			Blocks: workspace.Text("x"),
		})
		if !kernel.IsCode(err, kernel.CodeNotFound) {
			t.Errorf("err = %v, want NOT_FOUND", err)
		}
	})
}

// TestRolesContentAndBlobs covers the owner-scoped tables the conversation
// snapshot reads from: a role a conversation can name, content it can
// attach, and bytes behind a blob row.
func TestRolesContentAndBlobs(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")

	role, err := handle.Roles().Put(ctx, domain.Role{Name: "guide", Persona: "Be brief."}, 0)
	if err != nil {
		t.Fatalf("Roles.Put: %v", err)
	}
	if role.ID == "" || role.Rev == 0 {
		t.Fatalf("role = %+v, want an ID and a revision", role)
	}
	got, err := handle.Roles().Get(ctx, role.ID)
	if err != nil || got.Persona != "Be brief." {
		t.Fatalf("Roles.Get = %+v, %v; want the persona back", got, err)
	}
	page, err := handle.Roles().List(ctx, store.RoleFilter{}, store.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("Roles.List = %d items, %v; want 1", len(page.Items), err)
	}
	// A conversation can name the role now that it exists.
	conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{RoleID: role.ID})
	if err != nil {
		t.Fatalf("Create with role: %v", err)
	}
	conv, err = handle.Conversations().Update(ctx, conv.ID, workspace.UpdateConversation{Title: strPtr("retitled")})
	if err != nil || conv.Title != "retitled" {
		t.Fatalf("Update = %+v, %v; want the new title", conv, err)
	}
	if err := handle.Roles().Delete(ctx, role.ID, role.Rev); err != nil {
		t.Fatalf("Roles.Delete: %v", err)
	}
	if _, err := handle.Roles().Get(ctx, role.ID); !kernel.IsCode(err, kernel.CodeNotFound) {
		t.Errorf("Get after delete = %v, want NOT_FOUND", err)
	}

	// Content versions forward and tombstone hides the key.
	v1, err := handle.Content().Put(ctx, domain.Content{
		Key: "notes", Type: "text", Mode: domain.ModeRead, Value: json.RawMessage(`{"n":1}`),
	}, 0)
	if err != nil || v1.Version != 1 {
		t.Fatalf("Content.Put v1 = %d, %v; want version 1", v1.Version, err)
	}
	v2, err := handle.Content().Put(ctx, domain.Content{
		Key: "notes", Type: "text", Mode: domain.ModeRead, Value: json.RawMessage(`{"n":2}`),
	}, 1)
	if err != nil || v2.Version != 2 {
		t.Fatalf("Content.Put v2 = %d, %v; want version 2", v2.Version, err)
	}
	latest, err := handle.Content().Get(ctx, domain.ContentRef{Key: "notes"})
	if err != nil || latest.Version != 2 {
		t.Fatalf("Content.Get = version %d, %v; want the latest (2)", latest.Version, err)
	}
	if err := handle.Content().Tombstone(ctx, "notes", 2); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if _, err := handle.Content().Get(ctx, domain.ContentRef{Key: "notes"}); !kernel.IsCode(err, kernel.CodeNotFound) {
		t.Errorf("Get after tombstone = %v, want NOT_FOUND", err)
	}

	// Bytes first, metadata second: upload, then read the bytes back.
	blobRow, err := handle.Blobs().Upload(ctx, bytes.NewReader([]byte("blob bytes")), workspace.UploadOptions{
		MediaType: "text/plain", Name: "note.txt",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !strings.HasPrefix(blobRow.Checksum, "sha256:") {
		t.Errorf("checksum = %q, want a sha256 digest", blobRow.Checksum)
	}
	rc, err := handle.Blobs().Open(ctx, blobRow.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	gotBytes, err := io.ReadAll(rc)
	if err != nil || string(gotBytes) != "blob bytes" {
		t.Errorf("Open read %q, %v; want the uploaded bytes", gotBytes, err)
	}
	// The same bytes again are the same blob: content addressing dedupes.
	again, err := handle.Blobs().Upload(ctx, bytes.NewReader([]byte("blob bytes")), workspace.UploadOptions{MediaType: "text/plain"})
	if err != nil {
		t.Fatalf("second Upload: %v", err)
	}
	if again.ID != blobRow.ID {
		t.Errorf("second upload minted %s, want the existing %s", again.ID, blobRow.ID)
	}
}

// TestAdminReportsHealth is the maintenance surface: health reads what the
// store probes, sweep reports what it removed.
func TestAdminReportsHealth(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")
	conv, err := handle.Conversations().Create(ctx, workspace.CreateConversation{Title: "sweep me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := handle.Turns().Append(ctx, conv.ID, workspace.AppendTurn{
		Actor:  domain.Actor{Kind: domain.ActorUser, ID: "test"},
		Blocks: workspace.Text("keep me"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	health, err := h.WS.Admin().Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.OK {
		t.Errorf("health = %+v, want OK", health)
	}
	if !strings.Contains(health.Store, "memory") {
		t.Errorf("store = %q, want the memory adapter", health.Store)
	}

	// Everything is younger than the sweep grace, so nothing is removed.
	summary, err := h.WS.Admin().Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if summary.Turns != 0 || summary.Blobs != 0 || summary.Receipts != 0 {
		t.Errorf("sweep removed %+v from a fresh workspace, want nothing", summary)
	}
	chain := chainOf(t, handle, conv.ID)
	if len(chain) != 1 {
		t.Errorf("sweep left %d turns, want the conversation intact", len(chain))
	}
}

// TestAdminSweepReconcilesOrphanBytes is the Phase 2 gate item end to end:
// bytes uploaded but never registered (the crash window between step 1 and
// step 2 of a blob upload) are removed by the admin sweep once the grace
// passes, while bytes whose checksum has metadata under any owner survive.
func TestAdminSweepReconcilesOrphanBytes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	handle := h.As("test")

	// The orphan: bytes written, crash before register.
	orphan, err := h.Blobs.Put(ctx, bytes.NewReader([]byte("crashed mid-upload")),
		blob.PutOptions{MediaType: "text/plain"})
	if err != nil {
		t.Fatalf("Put orphan: %v", err)
	}

	// The survivor: a complete upload (bytes + metadata).
	kept, err := handle.Blobs().Upload(ctx, bytes.NewReader([]byte("complete upload")),
		workspace.UploadOptions{MediaType: "text/plain", Name: "kept.txt"})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Inside the sweep grace: nothing goes.
	first, err := h.WS.Admin().Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep 1: %v", err)
	}
	if first.Bytes != 0 {
		t.Errorf("first sweep removed %d bytes inside grace, want 0", first.Bytes)
	}
	if _, err := h.Blobs.Stat(ctx, orphan.Checksum); err != nil {
		t.Fatalf("orphan removed inside grace: %v", err)
	}

	// Past the grace (24h default): only the orphan goes.
	h.Clock.Advance(25 * time.Hour)
	second, err := h.WS.Admin().Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep 2: %v", err)
	}
	if second.Bytes != 1 {
		t.Errorf("second sweep removed %d bytes, want 1", second.Bytes)
	}
	if _, err := h.Blobs.Stat(ctx, orphan.Checksum); err == nil {
		t.Errorf("orphan bytes still present after the grace passed")
	}
	if _, err := h.Blobs.Stat(ctx, kept.Checksum); err != nil {
		t.Errorf("referenced bytes deleted: %v", err)
	}

	// A third sweep is quiet.
	third, err := h.WS.Admin().Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep 3: %v", err)
	}
	if third.Bytes != 0 {
		t.Errorf("steady-state sweep removed %d bytes, want 0", third.Bytes)
	}
}

// TestOpenValidatesConfiguration rejects an unusable configuration before
// anything is wired, with INVALID for a missing dependency and CONFLICT for
// a tool registered twice.
func TestOpenValidatesConfiguration(t *testing.T) {
	ctx := context.Background()
	valid := func() workspace.Config {
		return workspace.Config{
			Store:      memory.New(memory.Options{}),
			Blobs:      workspacetest.NewMemoryBlobs(),
			Providers:  []provider.Provider{workspacetest.NewScriptedProvider("scripted")},
			Authorizer: kernel.AllowOwner(),
		}
	}

	cases := []struct {
		name   string
		code   kernel.Code
		mutate func(*workspace.Config)
	}{
		{"no store", kernel.CodeInvalid, func(c *workspace.Config) { c.Store = nil }},
		{"no blobs", kernel.CodeInvalid, func(c *workspace.Config) { c.Blobs = nil }},
		{"no providers", kernel.CodeInvalid, func(c *workspace.Config) { c.Providers = nil }},
		{"no authorizer", kernel.CodeInvalid, func(c *workspace.Config) { c.Authorizer = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)
			if _, err := workspace.Open(ctx, cfg); !kernel.IsCode(err, tc.code) {
				t.Errorf("err = %v, want %s", err, tc.code)
			}
		})
	}

	t.Run("the same tool twice is a conflict", func(t *testing.T) {
		cfg := valid()
		tool := agent.NewTool(agent.ToolSpec{Name: "dup"}, func(ctx context.Context, p kernel.Principal, in struct{}) (struct{}, error) {
			return struct{}{}, nil
		})
		cfg.Tools = []agent.Tool{tool, tool}
		if _, err := workspace.Open(ctx, cfg); !kernel.IsCode(err, kernel.CodeConflict) {
			t.Errorf("err = %v, want CONFLICT", err)
		}
	})

	t.Run("a cancelled context is a cancellation", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := workspace.Open(cancelled, valid()); !kernel.IsCode(err, kernel.CodeCanceled) {
			t.Errorf("err = %v, want CANCELED", err)
		}
	})
}

// --- helpers ---

func withTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func describe(chain []domain.Turn) string {
	parts := make([]string, 0, len(chain))
	for _, turn := range chain {
		parts = append(parts, fmt.Sprintf("%s:v%d", turn.Actor.Kind, turn.Version))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// requestContains reports whether any part of the request carries needle —
// as message text, as a tool call's arguments or as a tool result's
// output, which is how the transcript reaches the provider.
func requestContains(req provider.Request, needle string) bool {
	for _, msg := range req.Messages {
		if partsContain(msg.Parts, needle) {
			return true
		}
	}
	return partsContain(req.System, needle)
}

func partsContain(parts []provider.Part, needle string) bool {
	for _, part := range parts {
		switch p := part.(type) {
		case provider.TextPart:
			if strings.Contains(p.Text, needle) {
				return true
			}
		case provider.ToolCallPart:
			if strings.Contains(string(p.Arguments), needle) {
				return true
			}
		case provider.ToolResultPart:
			if strings.Contains(string(p.Output), needle) {
				return true
			}
		}
	}
	return false
}

func describeRequest(req provider.Request) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			if text, ok := part.(provider.TextPart); ok {
				b.WriteString(text.Text)
				b.WriteString(" | ")
			}
		}
	}
	return b.String()
}

func strPtr(s string) *string { return &s }
