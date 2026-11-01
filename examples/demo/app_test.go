package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asaidimu/workspace"
	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store/memory"
	"github.com/asaidimu/workspace/workspacetest"
)

// newTestApp opens a workspace on the scripted provider — the demo's real
// wiring minus the network — and returns the app at the model picker.
func newTestApp(t *testing.T) (*app, *workspacetest.ScriptedProvider) {
	t.Helper()
	ctx := context.Background()

	sp := workspacetest.NewScriptedProvider("scripted")
	ws, err := workspace.Open(ctx, workspace.Config{
		Store:      memory.New(memory.Options{}),
		Blobs:      workspacetest.NewMemoryBlobs(),
		Providers:  []provider.Provider{sp},
		Authorizer: kernel.AllowOwner(),
		Tools:      []agent.Tool{shellTool(Config{Root: "."})},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close(context.Background()) })

	models, err := sp.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	a := newApp(appConfig{
		handle:       ws.For(workspace.LocalUser("test")),
		owner:        "test",
		instructions: "be brief",
		models:       models,
	})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return a, sp
}

// waitForRun drives the app's own poll path until the run settles, yielding
// to the loop's goroutine rather than sleeping.
func waitForRun(t *testing.T, a *app) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.runID != "" {
		if time.Now().After(deadline) {
			t.Fatalf("run never settled (status %q, notice %q)", a.status, a.notice)
		}
		a.Update(pollRunMsg{runID: a.runID})
		runtime.Gosched()
	}
}

func TestPickerSelectsAndConversationReplies(t *testing.T) {
	a, sp := newTestApp(t)
	sp.ReplyText("hello from the model")

	if a.phase != phaseModels {
		t.Fatalf("phase = %v, want the model picker", a.phase)
	}

	// Down and back up exercises the cursor, then enter selects.
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if a.phase != phaseChat {
		t.Fatalf("phase = %v after enter, want chat", a.phase)
	}
	if a.convID == "" || a.modelID != "scripted" {
		t.Fatalf("convID %q model %q, want a conversation on scripted", a.convID, a.modelID)
	}

	for _, r := range "hello workspace" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.runID == "" {
		t.Fatalf("enter did not start a run (notice %q)", a.notice)
	}

	waitForRun(t, a)

	if a.failure != "" {
		t.Fatalf("run failed: %s", a.failure)
	}
	if len(a.transcript) != 2 {
		t.Fatalf("transcript has %d turns, want user + agent: %+v", len(a.transcript), a.transcript)
	}
	if got := turnBody(a.transcript[0]); got != "hello workspace" {
		t.Errorf("user turn = %q, want the typed message", got)
	}
	if got := turnBody(a.transcript[1]); got != "hello from the model" {
		t.Errorf("agent turn = %q, want the provider reply", got)
	}

	view := a.View()
	for _, want := range []string{"workspace demo", "agent", "hello from the model", "conv " + a.convID} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q:\n%s", want, view)
		}
	}

	// The provider saw exactly one request carrying the user's text.
	reqs := sp.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times, want 1", len(reqs))
	}
}

func TestFailedRunShowsTheFailure(t *testing.T) {
	a, _ := newTestApp(t) // no scripted reply: the run fails

	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "doomed" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.runID == "" {
		t.Fatalf("enter did not start a run (notice %q)", a.notice)
	}

	waitForRun(t, a)

	if a.failure == "" {
		t.Fatal("a run that exhausted the script reported no failure")
	}
	if !strings.Contains(a.View(), a.failure) {
		t.Errorf("view does not show the failure %q:\n%s", a.failure, a.View())
	}
	// The user's turn is committed even though the run failed.
	if len(a.transcript) != 1 || turnBody(a.transcript[0]) != "doomed" {
		t.Errorf("transcript = %+v, want just the committed user turn", a.transcript)
	}
}

func TestTypedBlocksRenderAsReadableText(t *testing.T) {
	turn := domain.Turn{Blocks: []domain.Block{
		{Type: "document", Value: map[string]any{"title": "Blocks", "text": "document, json, thinking"}},
		{Type: "json", Value: map[string]any{"blocks": []any{"document", "json"}}},
		{Type: "thinking", Value: map[string]any{"text": "the user said hello"}},
	}}
	got := turnBody(turn)
	for _, want := range []string{
		"Blocks\n\ndocument, json, thinking",
		"\"blocks\": [\n    \"document\",\n    \"json\"\n  ]",
		"the user said hello",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("turnBody is missing %q:\n%s", want, got)
		}
	}
}

func TestUnknownBlocksRenderAsTheirText(t *testing.T) {
	turn := domain.Turn{Blocks: []domain.Block{
		{Type: "unknown", Raw: []byte(`{"title":"Blocks","content":"document, json"}`)},
		{Type: "text", Value: map[string]any{"text": "still prose"}},
	}}
	got := turnBody(turn)
	want := "{\"title\":\"Blocks\",\"content\":\"document, json\"}\nstill prose"
	if got != want {
		t.Errorf("turnBody = %q, want %q", got, want)
	}

	// A part the adapter could not type keeps its value's raw member.
	part := domain.Turn{Blocks: []domain.Block{{
		Type:  "unknown",
		Value: map[string]any{"raw": "mystery part", "originalType": "*provider.Foo"},
	}}}
	if got := turnBody(part); got != "mystery part" {
		t.Errorf("turnBody = %q, want the preserved raw value", got)
	}
}

func TestRetryStartsAFreshRunAfterFailure(t *testing.T) {
	a, sp := newTestApp(t)
	// The empty script makes the first attempt fail; queue the reply only
	// afterwards, so the retry is what consumes it.
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "hello" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	waitForRun(t, a)
	if a.failure == "" {
		t.Fatal("the first run did not fail")
	}

	sp.ReplyText("second try")
	a.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if a.runID == "" {
		t.Fatalf("ctrl+r did not start a retry (notice %q)", a.notice)
	}
	waitForRun(t, a)
	if a.failure != "" {
		t.Fatalf("retry failed: %s", a.failure)
	}
	if len(a.transcript) != 2 {
		t.Fatalf("transcript has %d turns, want the user turn plus the retried reply", len(a.transcript))
	}
	if got := turnBody(a.transcript[1]); got != "second try" {
		t.Errorf("retried agent turn = %q, want %q", got, "second try")
	}
	if got := len(sp.Requests()); got != 2 {
		t.Errorf("provider saw %d requests, want the failed attempt and the retry", got)
	}
}

func TestEventsReachTheFooter(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	a.Update(eventMsg{env: kernel.EventEnvelope{Name: "run.started", At: time.Now()}})
	if !strings.Contains(a.View(), "run.started") {
		t.Errorf("footer does not show the event:\n%s", a.View())
	}
}

// waitForStatus blocks until the run reaches want, driving no UI polls so the
// test observes the run the way the approval panel would.
func waitForStatus(t *testing.T, a *app, want domain.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := a.cfg.handle.Runs().Get(context.Background(), a.runID)
		if err == nil && run.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached %q (status %q)", want, run.Status)
		}
		time.Sleep(time.Millisecond)
	}
}

// parkOnShell starts a run whose first reply calls the shell, and returns
// once the run is parked. The approval event the manager emits is delivered
// by the real event pump in main; here it is injected directly.
func parkOnShell(t *testing.T, a *app, sp *workspacetest.ScriptedProvider, command string) {
	t.Helper()
	sp.ReplyToolCalls(provider.ToolCallPart{
		CallID:    "call-1",
		Name:      "shell",
		Arguments: json.RawMessage(`{"command":` + strconv.Quote(command) + `}`),
	})
	sp.ReplyText("done")

	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "run it" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.runID == "" {
		t.Fatal("send did not start a run")
	}
	waitForStatus(t, a, domain.RunWaiting)
	a.Update(eventMsg{env: kernel.EventEnvelope{
		Name:  "run.approval_requested",
		Event: agent.ApprovalRequested{RunID: a.runID, CallID: "call-1", Tool: "shell"},
		At:    time.Now(),
	}})
	if len(a.pending) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(a.pending))
	}
}

func TestShellApprovalRunsTheCommand(t *testing.T) {
	a, sp := newTestApp(t)
	parkOnShell(t, a, sp, "printf hi")

	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := a.View()
	t.Logf("\n%s", view)
	for _, want := range []string{"permission required", "shell", "printf hi", "y approve"} {
		if !strings.Contains(view, want) {
			t.Errorf("approval view is missing %q:\n%s", want, view)
		}
	}
	if got := len(strings.Split(view, "\n")); got != 30 {
		t.Errorf("approval view has %d lines, want 30", got)
	}
	for i, line := range strings.Split(view, "\n") {
		if got := lipgloss.Width(line); got > 100 {
			t.Errorf("line %d is %d columns wide, want at most 100: %q", i, got, line)
		}
	}

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if len(a.pending) != 0 {
		t.Fatalf("approval was not cleared: %+v", a.pending)
	}
	waitForRun(t, a)
	if a.failure != "" {
		t.Fatalf("run failed: %s", a.failure)
	}

	var toolOut map[string]any
	for _, turn := range a.transcript {
		for _, b := range turn.Blocks {
			if b.Type == "tool_result" {
				toolOut = asObject(blockObject(b)["output"])
			}
		}
	}
	if toolOut == nil {
		t.Fatalf("no tool result was committed: %+v", a.transcript)
	}
	if got, _ := toolOut["stdout"].(string); got != "hi" {
		t.Errorf("stdout = %q, want %q", got, "hi")
	}
	if got := turnBody(a.transcript[len(a.transcript)-1]); got != "done" {
		t.Errorf("final agent turn = %q, want %q", got, "done")
	}
	if got := len(sp.Requests()); got != 2 {
		t.Errorf("provider saw %d requests, want the tool call and the follow-up", got)
	}
}

func TestShellRejectionTellsTheModel(t *testing.T) {
	a, sp := newTestApp(t)
	parkOnShell(t, a, sp, "rm -rf /tmp/never")

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	waitForRun(t, a)
	if a.failure != "" {
		t.Fatalf("run failed: %s", a.failure)
	}

	var rejected bool
	for _, turn := range a.transcript {
		for _, b := range turn.Blocks {
			if b.Type != "tool_result" {
				continue
			}
			m := blockObject(b)
			if isErr, _ := m["is_error"].(bool); isErr {
				rejected = strings.Contains(fmt.Sprint(m["output"]), "rejected")
			}
		}
	}
	if !rejected {
		t.Errorf("the model was not told the call was rejected: %+v", a.transcript)
	}
}

func TestSlashPaletteFiltersCompletesAndRuns(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	view := a.View()
	t.Logf("\n%s", view)
	if got := len(strings.Split(view, "\n")); got != 30 {
		t.Errorf("palette view has %d lines, want 30", got)
	}
	for _, want := range []string{"/help", "/new", "/model", "/sessions", "/retry", "/quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("palette is missing %q:\n%s", want, view)
		}
	}

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mo")})
	view = a.View()
	if !strings.Contains(view, "/model") || strings.Contains(view, "/help") {
		t.Errorf("palette did not filter to /model:\n%s", view)
	}

	a.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := a.input.Value(); got != "/model " {
		t.Fatalf("tab completed to %q, want /model ", got)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.dialog != dialogModel {
		t.Fatalf("/model opened %q, want the model dialog", a.dialog)
	}
	view = a.View()
	if !strings.Contains(view, "Select model") || !strings.Contains(view, "scripted") {
		t.Errorf("model dialog is missing its list:\n%s", view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.dialog != dialogNone {
		t.Errorf("esc did not close the dialog, got %q", a.dialog)
	}
}

func TestSlashNewRotatesTheConversation(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	old := a.convID

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/new")})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if a.convID == "" || a.convID == old {
		t.Errorf("conversation id = %q, want a fresh one (was %q)", a.convID, old)
	}
	if len(a.transcript) != 0 {
		t.Errorf("transcript has %d turns, want a fresh conversation", len(a.transcript))
	}
	if a.phase != phaseChat {
		t.Errorf("phase = %v, want to stay in chat", a.phase)
	}
}

func TestUnknownSlashCommandIsNotSent(t *testing.T) {
	a, sp := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/nope")})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if !strings.Contains(a.notice, "unknown command") {
		t.Errorf("notice = %q, want an unknown-command message", a.notice)
	}
	if got := len(sp.Requests()); got != 0 {
		t.Errorf("provider saw %d requests, want none", got)
	}
	if len(a.transcript) != 0 {
		t.Errorf("transcript = %+v, want nothing committed", a.transcript)
	}
}

func TestSlashQuitLeavesTheDemo(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/quit")})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if !a.quitting {
		t.Error("/quit did not set quitting")
	}
}

func TestMarkdownRendererFormatsBlocks(t *testing.T) {
	out := strings.Join(renderMarkdown("## Blocks\n\n- `document`\n- **json**\n\n```go\nfmt.Println(1)\n```", 40), "\n")
	for _, want := range []string{"Blocks", "• ", "document", "json", "fmt.Println(1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown output is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"## ", "- `", "**", "```", "\x1b"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("markdown output still contains %q:\n%s", unwanted, out)
		}
	}

	// The numbered form glamour adds for free over the old renderer.
	out = strings.Join(renderMarkdown("1. one\n2. two", 40), "\n")
	if !strings.Contains(out, "1. one") || !strings.Contains(out, "2. two") {
		t.Errorf("numbered list did not render:\n%s", out)
	}
}

func TestComposerGrowsWithNewlines(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("first")})
	a.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second")})

	if got := a.input.Value(); got != "first\nsecond" {
		t.Errorf("input = %q, want the two lines", got)
	}
	if a.inputLines != 2 {
		t.Errorf("inputLines = %d, want the composer to have grown", a.inputLines)
	}
}

func TestThinkingRowShowsWhileRunning(t *testing.T) {
	a, sp := newTestApp(t)
	sp.ReplyText("later")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "hello" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if a.runID == "" {
		t.Fatal("send did not start a run")
	}
	if view := a.View(); !strings.Contains(view, "thinking") {
		t.Errorf("view does not show the in-flight row:\n%s", view)
	}
	waitForRun(t, a)
}

func TestChatLayoutFillsTheWindow(t *testing.T) {
	a, sp := newTestApp(t)
	sp.ReplyText("hello there")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "hi" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	waitForRun(t, a)

	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := a.View()
	t.Logf("\n%s", view)
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		if got := lipgloss.Width(line); got > a.width {
			t.Errorf("line %d is %d columns wide, window is %d: %q", i, got, a.width, line)
		}
	}
	if got := len(lines); got != 30 {
		t.Errorf("view has %d lines, want 30", got)
	}
	if !strings.Contains(view, "conv "+a.convID) {
		t.Errorf("header lost the conversation id:\n%s", view)
	}
}

// TestEnterQueuesWhileRunInFlight covers the opencode input behaviour: what
// you type while the agent works queues up and auto-sends when the run
// finishes, one message per run.
func TestEnterQueuesWhileRunInFlight(t *testing.T) {
	a, sp := newTestApp(t)
	sp.ReplyText("one")
	sp.ReplyText("two")

	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "hi" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.runID == "" {
		t.Fatal("send did not start a run")
	}

	for _, r := range "again" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.queued) != 1 || a.queued[0] != "again" {
		t.Fatalf("queued = %v, want [again]", a.queued)
	}
	if got := a.input.Value(); got != "" {
		t.Errorf("composer kept %q after queueing", got)
	}
	if view := a.View(); !strings.Contains(view, "queued (1)") {
		t.Errorf("view does not show the queue:\n%s", view)
	}

	// Both runs settle inside the poll loop: the first one's terminal
	// poll starts the queued send.
	waitForRun(t, a)
	if len(a.queued) != 0 {
		t.Errorf("queue did not drain: %v", a.queued)
	}
	if len(a.transcript) != 4 {
		t.Fatalf("transcript has %d turns, want hi/one/again/two: %+v", len(a.transcript), a.transcript)
	}
	for turn, want := range map[int]string{0: "hi", 1: "one", 2: "again", 3: "two"} {
		if got := turnBody(a.transcript[turn]); got != want {
			t.Errorf("turn %d = %q, want %q", turn, got, want)
		}
	}
	if got := len(sp.Requests()); got != 2 {
		t.Errorf("provider saw %d requests, want one per queued message", got)
	}
}

// TestFailedRunHandsQueuedMessageBack makes sure a failure does not eat what
// the user queued: the first message returns to the composer for editing.
func TestFailedRunHandsQueuedMessageBack(t *testing.T) {
	a, _ := newTestApp(t) // empty script: the run fails

	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "hi" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	for _, r := range "later" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.queued) != 1 {
		t.Fatalf("queued = %v, want [later]", a.queued)
	}

	waitForRun(t, a)
	if a.failure == "" {
		t.Fatal("the run did not fail")
	}
	if got := a.input.Value(); got != "later" {
		t.Errorf("composer holds %q, want the queued message back", got)
	}
	if len(a.queued) != 0 {
		t.Errorf("queue still holds %v", a.queued)
	}
}

// TestTurnEventsReloadTheTranscript pins the liveness contract: a turn
// committed while the UI is idle appears as soon as its event arrives, and
// only for the conversation on screen.
func TestTurnEventsReloadTheTranscript(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	_, err := a.cfg.handle.Turns().Append(context.Background(), a.convID, workspace.AppendTurn{
		Actor:  domain.Actor{Kind: domain.ActorSystem, ID: "scripted"},
		Blocks: workspace.Text("committed behind the UI's back"),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(a.transcript) != 0 {
		t.Fatalf("transcript updated without an event: %+v", a.transcript)
	}

	a.Update(eventMsg{env: kernel.EventEnvelope{Name: "turn.appended", ConversationID: a.convID, At: time.Now()}})
	if len(a.transcript) != 1 || turnBody(a.transcript[0]) != "committed behind the UI's back" {
		t.Fatalf("transcript after event = %+v, want the committed turn", a.transcript)
	}

	a.Update(eventMsg{env: kernel.EventEnvelope{Name: "turn.appended", ConversationID: "another-conversation", At: time.Now()}})
	if len(a.transcript) != 1 {
		t.Errorf("an event for another conversation reloaded the transcript")
	}
}

// TestMouseWheelScrollsTheTranscript covers the mouse wheel: rolling up
// moves the viewport off the bottom.
func TestMouseWheelScrollsTheTranscript(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	for i := 0; i < 30; i++ {
		if _, err := a.cfg.handle.Turns().Append(context.Background(), a.convID, workspace.AppendTurn{
			Actor:  domain.Actor{Kind: domain.ActorUser, ID: "test"},
			Blocks: workspace.Text(fmt.Sprintf("message %d with enough words to wrap around a little", i)),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	a.reload()
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	atBottom := a.view.YOffset
	if atBottom == 0 {
		t.Fatal("transcript fits the viewport, nothing to scroll")
	}

	a.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	if a.view.YOffset >= atBottom {
		t.Errorf("wheel up did not scroll away from the bottom: %d (was %d)", a.view.YOffset, atBottom)
	}
	before := a.view.YOffset
	a.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	if a.view.YOffset >= before {
		t.Errorf("wheel up did not keep scrolling: %d then %d", before, a.view.YOffset)
	}
	scrolledUp := a.view.YOffset
	a.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if a.view.YOffset <= scrolledUp {
		t.Errorf("wheel down did not scroll back: %d after %d", a.view.YOffset, scrolledUp)
	}
}

// TestToolOutputCollapsesAndExpands pins the detail contract: a chatty
// command collapses to a preview with a hint, and ctrl+o reveals the rest.
func TestToolOutputCollapsesAndExpands(t *testing.T) {
	a, sp := newTestApp(t)
	parkOnShell(t, a, sp, "i=1; while [ $i -le 20 ]; do echo line$i; i=$((i+1)); done")

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	waitForRun(t, a)
	if a.failure != "" {
		t.Fatalf("run failed: %s", a.failure)
	}

	collapsed := a.View()
	if strings.Contains(collapsed, "line20") {
		t.Errorf("collapsed view leaked the output tail:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "ctrl+o") {
		t.Errorf("collapsed view has no expand hint:\n%s", collapsed)
	}

	a.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if !a.expand {
		t.Fatal("ctrl+o did not set the expand flag")
	}
	if expanded := a.View(); !strings.Contains(expanded, "line20") {
		t.Errorf("expanded view is missing the output tail:\n%s", expanded)
	}
}

// TestSessionsDialogSwitchesConversation covers the persisted-session
// surface: /sessions lists the conversations, and enter switches to the
// newest one with a reloaded transcript.
func TestSessionsDialogSwitchesConversation(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	first := a.convID

	second, err := a.cfg.handle.Conversations().Create(context.Background(), workspace.CreateConversation{
		Title: "second thread",
	})
	if err != nil {
		t.Fatalf("create second conversation: %v", err)
	}

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/sessions")})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.dialog != dialogSessions {
		t.Fatalf("dialog = %q, want sessions", a.dialog)
	}
	if view := a.View(); !strings.Contains(view, "second thread") {
		t.Errorf("sessions dialog does not list the new conversation:\n%s", view)
	}

	// Newest first, so enter lands on the second conversation.
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.dialog != dialogNone {
		t.Fatalf("dialog still %q after selecting", a.dialog)
	}
	if a.convID != second.ID {
		t.Fatalf("convID = %q, want the picked conversation %q (was %q)", a.convID, second.ID, first)
	}
	if len(a.transcript) != 0 {
		t.Errorf("transcript = %+v, want the empty second conversation", a.transcript)
	}
}

// TestHelpDialogOpensAndCloses replaces the old "type /" help hack.
func TestHelpDialogOpensAndCloses(t *testing.T) {
	a, _ := newTestApp(t)
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/help")})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.dialog != dialogHelp {
		t.Fatalf("dialog = %q, want help", a.dialog)
	}
	view := a.View()
	for _, want := range []string{"Commands", "/sessions", "ctrl+o"} {
		if !strings.Contains(view, want) {
			t.Errorf("help dialog is missing %q:\n%s", want, view)
		}
	}

	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.dialog != dialogNone {
		t.Errorf("esc did not close the help dialog")
	}
}
