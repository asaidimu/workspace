package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asaidimu/workspace"
	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/kernel"
	"github.com/asaidimu/workspace/provider"
	"github.com/asaidimu/workspace/store"
)

// phase is which screen the program is on. Once a conversation is open the
// chat screen hosts everything; pickers and help are overlays on it.
type phase int

const (
	phaseModels phase = iota
	phaseChat
)

// dialog names the overlay open over the chat screen, if any.
const (
	dialogNone     = ""
	dialogModel    = "model"
	dialogSessions = "sessions"
	dialogHelp     = "help"
)

// Messages from outside the Update loop: workspace events, run polls, and
// the window size tea itself sends.
type eventMsg struct{ env kernel.EventEnvelope }
type pollRunMsg struct{ runID string }

// approvalRequest is one RiskConfirm call waiting for a person. The agent
// commits the assistant turn (including the call's arguments) before it
// parks, so the demo can show the exact command from the transcript.
type approvalRequest struct {
	RunID  string
	CallID string
	Tool   string
	Args   map[string]any
}

type appConfig struct {
	handle       *workspace.Handle
	owner        string
	instructions string
	models       []provider.ModelInfo
	// sessions are the persisted conversations, newest first. When non-empty
	// the first one resumes after the model pick instead of creating a fresh
	// conversation.
	sessions []domain.Conversation
}

type app struct {
	cfg appConfig

	phase     phase
	cursor    int
	cmdCursor int

	modelID string
	convID  string
	runID   string // empty when no run is in flight

	input textarea.Model
	view  viewport.Model
	spin  spinner.Model

	width, height int
	inputLines    int

	transcript []domain.Turn
	pending    []approvalRequest
	events     []string
	lastEvent  string
	status     string
	failure    string
	notice     string
	usage      domain.Usage

	// queued holds messages typed while a run was in flight; the poll loop
	// sends them FIFO once the run succeeds.
	queued []string

	// dialog is which overlay is open (dialogNone when none), with its
	// cursor and type-to-filter text.
	dialog       string
	dialogCursor int
	dialogFilter string

	// expand toggles collapsed tool output and thinking blocks (ctrl+o).
	expand bool

	quitting bool
}

func newApp(cfg appConfig) *app {
	in := textarea.New()
	in.Placeholder = "Ask the workspace anything…"
	in.CharLimit = 4096
	in.ShowLineNumbers = false
	in.MaxHeight = 12
	in.FocusedStyle.CursorLine = lipgloss.NewStyle()
	in.BlurredStyle.CursorLine = lipgloss.NewStyle()
	in.FocusedStyle.Prompt = accentStyle
	in.BlurredStyle.Prompt = mutedStyle
	in.FocusedStyle.Placeholder = mutedStyle
	in.BlurredStyle.Placeholder = mutedStyle
	in.FocusedStyle.Text = textStyle
	in.BlurredStyle.Text = mutedStyle
	in.SetPromptFunc(2, func(i int) string {
		if i == 0 {
			return "❯ "
		}
		return "  "
	})

	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	sp.Style = accentStyle

	return &app{
		cfg:    cfg,
		input:  in,
		view:   viewport.New(80, 20),
		spin:   sp,
		status: "choose a model",
	}
}

func (a *app) Init() tea.Cmd { return a.spin.Tick }

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.syncLayout()
		return a, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		a.spin, cmd = a.spin.Update(msg)
		return a, cmd

	case tea.MouseMsg:
		// Mouse wheel scrolls the transcript; dialogs and the picker
		// ignore it.
		if a.phase == phaseChat && a.dialog == dialogNone {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				a.view.ScrollUp(1)
			case tea.MouseButtonWheelDown:
				a.view.ScrollDown(1)
			}
		}
		return a, nil

	case eventMsg:
		return a.event(msg)

	case pollRunMsg:
		return a.poll(msg.runID)

	case tea.KeyMsg:
		return a.key(msg)
	}
	return a, nil
}

// event records one workspace envelope. Turn events for the open
// conversation reload the transcript immediately, so tool calls and results
// appear while the run is still working — the gap opencode never leaves.
func (a *app) event(msg eventMsg) (tea.Model, tea.Cmd) {
	env := msg.env
	line := fmt.Sprintf("%s %s", env.At.Format("15:04:05"), env.Name)
	a.lastEvent = line
	a.events = append(a.events, line)
	if len(a.events) > 200 {
		a.events = a.events[len(a.events)-200:]
	}
	if req, ok := env.Event.(agent.ApprovalRequested); ok {
		a.reload()
		a.addApproval(req)
		return a, nil
	}
	if a.phase == phaseChat && env.ConversationID != "" && env.ConversationID == a.convID &&
		strings.HasPrefix(env.Name, "turn.") {
		a.reload()
	}
	return a, nil
}

// key routes one key press: global keys first, then the current screen.
func (a *app) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		a.cancelRun()
		a.quitting = true
		return a, tea.Quit
	}

	if a.phase == phaseModels {
		return a.modelKeys(msg)
	}
	return a.chatKeys(msg)
}

func (a *app) modelKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		a.cursor--
		if a.cursor < 0 {
			a.cursor = len(a.cfg.models) - 1
		}
	case "down", "j":
		a.cursor++
		if a.cursor >= len(a.cfg.models) {
			a.cursor = 0
		}
	case "enter":
		return a, a.selectModel()
	case "esc", "q":
		a.quitting = true
		return a, tea.Quit
	}
	return a, nil
}

func (a *app) chatKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(a.pending) > 0 {
		switch msg.String() {
		case "y":
			return a.resolveApproval(true)
		case "n":
			return a.resolveApproval(false)
		case "esc":
			a.cancelRun()
			return a, nil
		}
		// The composer is replaced by the permission panel, so keystrokes
		// must not accumulate in the hidden input.
		return a, nil
	}
	if a.dialog != dialogNone {
		return a.dialogKeys(msg)
	}
	if a.paletteVisible() {
		matches := a.paletteMatches()
		switch msg.String() {
		case "up":
			a.cmdCursor = (min(a.cmdCursor, len(matches)-1) - 1 + len(matches)) % len(matches)
			return a, nil
		case "down":
			a.cmdCursor = (min(a.cmdCursor, len(matches)-1) + 1) % len(matches)
			return a, nil
		case "tab":
			a.input.SetValue(matches[min(a.cmdCursor, len(matches)-1)].name + " ")
			a.cmdCursor = 0
			a.syncLayout()
			return a, nil
		case "esc":
			// Close the palette instead of leaving the demo.
			a.input.SetValue("")
			a.cmdCursor = 0
			a.syncLayout()
			return a, nil
		}
	}

	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(a.input.Value())
		if text == "" {
			return a, nil
		}
		if strings.HasPrefix(text, "/") {
			return a.runCommand(text)
		}
		if a.runID != "" {
			// opencode behaviour: while the agent works, what you type
			// queues and sends when the run finishes.
			a.queued = append(a.queued, text)
			a.input.SetValue("")
			a.syncLayout()
			return a, nil
		}
		return a.send(text)
	case "ctrl+j":
		a.input.InsertString("\n")
		a.syncLayout()
		return a, nil
	case "ctrl+o":
		a.expand = !a.expand
		a.renderTranscript()
		return a, nil
	case "ctrl+r":
		return a.retry()
	case "esc":
		// Cancel the run in flight, or quit when idle.
		if a.runID != "" {
			a.cancelRun()
			return a, nil
		}
		a.quitting = true
		return a, tea.Quit
	case "ctrl+u":
		a.view.HalfViewUp()
		return a, nil
	case "ctrl+d":
		a.view.HalfViewDown()
		return a, nil
	case "pgup":
		a.view.ViewUp()
		return a, nil
	case "pgdown":
		a.view.ViewDown()
		return a, nil
	}
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(msg)
	a.syncLayout()
	return a, cmd
}

// selectModel locks the chosen model in and opens the conversation the chat
// sends into: the newest persisted one when the store has history, a fresh
// conversation otherwise.
func (a *app) selectModel() tea.Cmd {
	m := a.cfg.models[a.cursor]
	a.modelID = m.ID

	if len(a.cfg.sessions) > 0 {
		a.convID = a.cfg.sessions[0].ID
		a.phase = phaseChat
		a.status = "ready"
		a.notice = ""
		a.syncLayout()
		a.reload()
		return tea.Batch(a.input.Focus())
	}

	conv, err := a.createConversation()
	if err != nil {
		a.notice = fmt.Sprintf("create conversation: %v", err)
		return nil
	}
	a.convID = conv.ID
	a.phase = phaseChat
	a.status = "ready"
	a.notice = ""
	a.syncLayout()
	return tea.Batch(a.input.Focus())
}

// createConversation opens the conversation a chat sends into.
func (a *app) createConversation() (domain.Conversation, error) {
	return a.cfg.handle.Conversations().Create(context.Background(), workspace.CreateConversation{
		Title: "demo " + time.Now().Format("15:04:05"),
	})
}

// send appends the user's turn and starts a run over it, then schedules the
// poll that watches the run to a terminal state.
func (a *app) send(text string) (tea.Model, tea.Cmd) {
	ctx := context.Background()
	if _, err := a.cfg.handle.Turns().Append(ctx, a.convID, workspace.AppendTurn{
		Actor:  domain.Actor{Kind: domain.ActorUser, ID: a.cfg.owner},
		Blocks: workspace.Text(text),
	}); err != nil {
		a.notice = fmt.Sprintf("append turn: %v", err)
		return a, nil
	}
	a.input.SetValue("")
	a.cmdCursor = 0
	a.notice = ""
	a.failure = ""
	a.pending = nil
	a.syncLayout()
	a.reload()

	run, err := a.cfg.handle.Runs().Start(ctx, a.convID, workspace.StartRun{
		Model:        a.modelID,
		Instructions: a.cfg.instructions,
	})
	if err != nil {
		a.notice = fmt.Sprintf("start run: %v", err)
		return a, nil
	}
	a.runID = run.ID
	a.status = "queued"
	a.renderTranscript()
	return a, tea.Batch(a.spin.Tick, pollAfter(250*time.Millisecond, run.ID))
}

// retry starts a fresh run over the same conversation after a failure. The
// failed run already committed the user's turn, so nothing is appended: the
// new run simply continues from the head, which is exactly a retry.
func (a *app) retry() (tea.Model, tea.Cmd) {
	if a.runID != "" || a.failure == "" {
		return a, nil
	}
	a.failure = ""
	a.notice = ""
	a.reload()
	run, err := a.cfg.handle.Runs().Start(context.Background(), a.convID, workspace.StartRun{
		Model:        a.modelID,
		Instructions: a.cfg.instructions,
	})
	if err != nil {
		a.notice = fmt.Sprintf("start run: %v", err)
		return a, nil
	}
	a.runID = run.ID
	a.status = "queued"
	a.renderTranscript()
	return a, tea.Batch(a.spin.Tick, pollAfter(250*time.Millisecond, run.ID))
}

// poll reads one run's status. While the run is alive it schedules the next
// poll — the run's terminal state has no event (section 11 only emits
// started/approval/resumed/cancelled), so the UI watches the record itself.
func (a *app) poll(runID string) (tea.Model, tea.Cmd) {
	if runID != a.runID {
		return a, nil // stale poll from a run that already ended
	}
	run, err := a.cfg.handle.Runs().Get(context.Background(), runID)
	if err != nil {
		a.notice = fmt.Sprintf("get run: %v", err)
		return a, nil
	}
	a.status = string(run.Status)
	switch {
	case run.Status.IsTerminal():
		a.runID = ""
		a.pending = nil
		if run.Status == domain.RunFailed {
			a.failure = run.Failure
		} else if run.Status == domain.RunCancelled {
			a.failure = "run cancelled"
		}
		a.reload()
		// Drain the queue on success; hand the first queued message back
		// for editing when the run did not make it.
		if run.Status == domain.RunSucceeded && len(a.queued) > 0 {
			next := a.queued[0]
			a.queued = a.queued[1:]
			return a.send(next)
		}
		if run.Status != domain.RunSucceeded && len(a.queued) > 0 {
			a.input.SetValue(a.queued[0])
			a.queued = a.queued[1:]
			a.syncLayout()
		}
		return a, nil
	case run.Status == domain.RunWaiting:
		// The approval event carries the call; keep polling so the run is
		// followed once a person answers.
		a.reload()
		return a, pollAfter(250*time.Millisecond, runID)
	default:
		return a, pollAfter(250*time.Millisecond, runID)
	}
}

func (a *app) cancelRun() {
	if a.runID == "" {
		return
	}
	// Best effort: the poll sees the terminal state either way.
	_ = a.cfg.handle.Runs().Cancel(context.Background(), a.runID)
}

// reload re-reads the conversation's active chain — user turns, committed
// agent turns and tool traffic — oldest first, and refreshes the session
// usage totals from the agent turns' usage records.
func (a *app) reload() {
	chain, err := a.cfg.handle.Turns().Chain(context.Background(), a.convID, store.ChainOptions{})
	if err != nil {
		a.notice = fmt.Sprintf("read chain: %v", err)
		return
	}
	a.transcript = chain
	a.sumUsage()
	a.renderTranscript()
}

// sumUsage totals the token and cost records the agent turns carry, for the
// footer.
func (a *app) sumUsage() {
	var u domain.Usage
	for _, t := range a.transcript {
		if t.Actor.Kind != domain.ActorAgent {
			continue
		}
		u.InputTokens += t.Usage.InputTokens
		u.OutputTokens += t.Usage.OutputTokens
		u.CostMicros += t.Usage.CostMicros
		if t.Usage.Currency != "" {
			u.Currency = t.Usage.Currency
		}
	}
	a.usage = u
}

// syncLayout sizes the transcript and composer for the current window and
// input height. The composer grows with the text (up to six rows), the
// queue rows and the transcript share what is left.
func (a *app) syncLayout() {
	if a.width == 0 {
		a.width, a.height = 80, 24
	}
	lines := 1 + strings.Count(a.input.Value(), "\n")
	a.inputLines = min(max(lines, 1), 6)

	composerWidth := max(a.width-4, 16)
	a.input.SetWidth(max(composerWidth-2, 8))
	a.input.SetHeight(a.inputLines)

	a.view.Width = max(a.width, 20)
	a.view.Height = max(a.height-a.composerHeight()-a.paletteHeight()-a.queueHeight()-4, 3)
	a.renderTranscript()
}

// renderTranscript rebuilds the viewport's content from the chain. It keeps
// the scroll position when the user has scrolled away from the bottom, and
// pins to the newest message otherwise.
func (a *app) renderTranscript() {
	atBottom := a.view.AtBottom()
	width := max(a.width-2, 20)

	rc := renderCtx{width: width, expand: a.expand, runActive: a.runID != "", done: map[string]bool{}}
	for _, t := range a.transcript {
		for _, b := range t.Blocks {
			if b.Type == "tool_result" {
				rc.done[blockString(b, "call_id")] = true
			}
		}
	}

	var b strings.Builder
	for i, t := range a.transcript {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(renderTurn(t, rc))
	}
	if a.runID != "" && len(a.pending) == 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(accentStyle.Render("●") + " " + a.spin.View() + " " +
			mutedStyle.Render("thinking…"))
	}
	if b.Len() == 0 {
		b.WriteString(mutedStyle.Render("no turns yet — say hello to get started"))
	}

	a.view.SetContent(b.String())
	if atBottom {
		a.view.GotoBottom()
	}
}

func (a *app) View() string {
	if a.quitting {
		return ""
	}
	if a.width == 0 {
		a.width, a.height = 80, 24
		a.syncLayout()
	}
	if a.phase == phaseModels {
		return a.modelsView()
	}
	chat := a.chatView()
	if a.dialog != dialogNone {
		if box := a.dialogView(); box != "" {
			return overlayCenter(chat, box, a.width, a.height)
		}
	}
	return chat
}

// headerView is the top bar: the wordmark on the left, the model and
// conversation on the right.
func (a *app) headerView() string {
	left := wordmarkStyle.Render("workspace") + " " + mutedStyle.Render("demo")
	conv := a.convID
	if a.width < 80 && len(conv) > 8 {
		conv = conv[:8] + "…"
	}
	right := mutedStyle.Render("model ") + textStyle.Render(a.modelID) +
		mutedStyle.Render("  ·  conv ") + mutedStyle.Render(conv)
	line := padBetween(left, right, max(a.width-2, 20))
	return headerStyle.Render(fillWidth(line, max(a.width-2, 20)))
}

// paletteHeight is the rows the slash-command suggestions take. It is zero
// when the palette is collapsed.
func (a *app) paletteHeight() int {
	if !a.paletteVisible() {
		return 0
	}
	return len(a.paletteMatches()) + 2
}

// queueHeight is the rows the queued-message list takes above the composer.
func (a *app) queueHeight() int {
	if a.phase != phaseChat || len(a.queued) == 0 || len(a.pending) > 0 {
		return 0
	}
	return min(len(a.queued), 3) + 1
}

// queueView lists what will send when the current run finishes.
func (a *app) queueView() string {
	if a.queueHeight() == 0 {
		return ""
	}
	shown := a.queued
	if len(shown) > 3 {
		shown = shown[len(shown)-3:]
	}
	var lines []string
	for _, q := range shown {
		lines = append(lines, faintStyle.Render("⏎ "+truncate(q, max(a.width-6, 8))))
	}
	lines = append(lines, mutedStyle.Render(fmt.Sprintf("queued (%d) — sends when the run finishes", len(a.queued))))
	return strings.Join(lines, "\n")
}

// composerHeight is the rows the composer area takes, border included. The
// permission panel has a fixed height so the transcript does not jump when
// it replaces the input.
func (a *app) composerHeight() int {
	if len(a.pending) > 0 {
		return 8
	}
	return a.inputLines + 2
}

// composerView is the rounded input box. Its border brightens when the
// workspace is idle and dims while a run is in flight.
func (a *app) composerView() string {
	if len(a.pending) > 0 {
		return a.approvalView()
	}
	style := composerActiveStyle
	if a.runID != "" {
		style = composerStyle
	}
	return style.Width(max(a.width-4, 16)).Render(a.input.View())
}

// approvalView is the permission panel that replaces the composer while a
// RiskConfirm call waits. It shows exactly what would run, because the
// assistant turn that requested it is already committed.
func (a *app) approvalView() string {
	p := a.pending[0]
	body := []string{
		errorStyle.Bold(true).Render("permission required"),
		textStyle.Render(p.Tool + " wants to run:"),
	}
	for _, line := range wrapText(approvalCommand(p), max(a.width-12, 10)) {
		if len(body) >= 5 {
			break
		}
		body = append(body, codeStyle.Render(" "+line))
	}
	body = append(body, mutedStyle.Render("y approve · n reject · esc cancel run"))
	for len(body) < 6 {
		body = append(body, "")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colErr).
		Padding(0, 1).
		Width(max(a.width-4, 16)).
		Render(strings.Join(body[:6], "\n"))
}

// approvalCommand is what the panel shows: the shell command line when the
// tool is the shell, otherwise the call's arguments as compact JSON.
func approvalCommand(p approvalRequest) string {
	if cmd, ok := p.Args["command"].(string); ok {
		if dir, ok := p.Args["workdir"].(string); ok && dir != "" {
			return "$ " + cmd + "  (in " + dir + ")"
		}
		return "$ " + cmd
	}
	if len(p.Args) == 0 {
		return p.Tool
	}
	raw, err := json.Marshal(p.Args)
	if err != nil {
		return p.Tool
	}
	return string(raw)
}

// addApproval queues one parked call, deduplicated by call ID.
func (a *app) addApproval(req agent.ApprovalRequested) {
	for _, p := range a.pending {
		if p.CallID == req.CallID {
			return
		}
	}
	name, args := findToolCall(a.transcript, req.CallID)
	if name == "" {
		name = req.Tool
	}
	a.pending = append(a.pending, approvalRequest{
		RunID:  req.RunID,
		CallID: req.CallID,
		Tool:   name,
		Args:   args,
	})
	a.syncLayout()
}

// resolveApproval answers the oldest pending call. The poll loop keeps
// running while the run is parked, so the resumed run needs no new command.
func (a *app) resolveApproval(approve bool) (tea.Model, tea.Cmd) {
	p := a.pending[0]
	a.pending = a.pending[1:]
	reason := ""
	if !approve {
		reason = "the user rejected the command"
	}
	if err := a.cfg.handle.Runs().Approve(context.Background(), p.RunID, p.CallID, approve, reason); err != nil {
		a.notice = fmt.Sprintf("approve: %v", err)
	}
	a.syncLayout()
	a.reload()
	return a, nil
}

// footerView is two lines: status and key hints, then the last event or the
// error that needs attention, with the session's token and cost totals on
// the right.
func (a *app) footerView() string {
	status, hints := a.statusLine()
	first := padBetween(status, mutedStyle.Render(hints), max(a.width, 20))

	usage := a.usageText()
	second := mutedStyle.Render("event: " + orNone(a.lastEvent))
	switch {
	case a.failure != "":
		second = errorStyle.Render("✗ " + a.failure)
	case a.notice != "":
		second = errorStyle.Render("✗ " + a.notice)
	}
	second = padBetween(truncate(second, max(a.width-lipgloss.Width(usage)-2, 20)), usage, max(a.width, 20))
	return first + "\n" + second
}

// usageText renders the session totals for the footer: the model's context
// window and the tokens and cost the conversation has spent.
func (a *app) usageText() string {
	var ctx int
	for _, m := range a.cfg.models {
		if m.ID == a.modelID {
			ctx = m.Capabilities.ContextWindow
			break
		}
	}
	parts := []string{}
	if ctx > 0 {
		parts = append(parts, "ctx "+compactCount(int64(ctx)))
	}
	if a.usage.InputTokens > 0 || a.usage.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d in / %d out", a.usage.InputTokens, a.usage.OutputTokens))
	}
	if a.usage.CostMicros > 0 {
		unit := a.usage.Currency
		if unit == "" {
			unit = "µ"
		}
		parts = append(parts, fmt.Sprintf("%.4f %s", float64(a.usage.CostMicros)/1e6, unit))
	}
	if len(parts) == 0 {
		return ""
	}
	return faintStyle.Render(strings.Join(parts, " · "))
}

func (a *app) statusLine() (status, hints string) {
	queue := ""
	if len(a.queued) > 0 {
		queue = fmt.Sprintf(" · %d queued", len(a.queued))
	}
	if len(a.pending) > 0 {
		return errorStyle.Render("◌ waiting for permission"), "y approve · n reject · esc cancel"
	}
	if a.runID != "" {
		return a.spin.View() + " " + textStyle.Render("working"), "esc cancel · ctrl+o detail · pgup/pgdn scroll"
	}
	if a.failure != "" || a.status == "failed" {
		return errorStyle.Render("✗ failed"), "ctrl+r retry · esc quit"
	}
	return okStyle.Render("● ready"), "enter send · / commands · ctrl+o detail · ctrl+j newline · esc quit" + queue
}

func (a *app) chatView() string {
	parts := []string{
		a.headerView(),
		a.view.View(),
		"",
	}
	if palette := a.paletteView(); palette != "" {
		parts = append(parts, palette)
	}
	if queue := a.queueView(); queue != "" {
		parts = append(parts, queue)
	}
	parts = append(parts, a.composerView(), a.footerView())
	return strings.Join(parts, "\n")
}

func (a *app) modelsView() string {
	panelWidth := min(max(a.width-12, 24), 64)
	var b strings.Builder
	b.WriteString(wordmarkStyle.Render("workspace") + " " + mutedStyle.Render("demo") + "\n")

	// OpenRouter's live catalogue alone runs to hundreds of models, so the
	// picker scrolls a window around the cursor instead of listing them all.
	header := "choose a model"
	if n := len(a.cfg.models); n > listWindow {
		header += fmt.Sprintf("  (%d/%d)", a.cursor+1, n)
	}
	b.WriteString(mutedStyle.Render(header) + "\n\n")

	lo, hi := windowAround(a.cursor, len(a.cfg.models), listWindow)
	if lo > 0 {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ↑ %d more", lo)) + "\n")
	}
	for i := lo; i < hi; i++ {
		m := a.cfg.models[i]
		id := fmt.Sprintf("%-22s", truncate(m.ID, 22))
		meta := describePricing(m)
		if i == a.cursor {
			row := fmt.Sprintf("▸ %s %s", id, meta)
			b.WriteString(lipgloss.NewStyle().
				Background(colPanel).
				Foreground(colAccent).
				Bold(true).
				Width(panelWidth).
				Render(truncate(row, panelWidth)) + "\n")
			continue
		}
		b.WriteString("  " + textStyle.Render(id) + " " + mutedStyle.Render(meta) + "\n")
	}
	if hi < len(a.cfg.models) {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ↓ %d more", len(a.cfg.models)-hi)) + "\n")
	}

	b.WriteString("\n" + mutedStyle.Render("↑/↓ move · enter select · q quit"))
	panel := pickerPanelStyle.Width(panelWidth).Render(strings.TrimRight(b.String(), "\n"))
	if a.notice != "" {
		panel += "\n" + errorStyle.Render(a.notice)
	}
	return lipgloss.Place(max(a.width, 1), max(a.height, 1), lipgloss.Center, lipgloss.Center, panel)
}

func pollAfter(d time.Duration, runID string) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return pollRunMsg{runID: runID} })
}

func describePricing(m provider.ModelInfo) string {
	ctx := compactCount(int64(m.Capabilities.ContextWindow))
	p := m.Pricing
	if p.InputMicrosPerMTok == 0 && p.OutputMicrosPerMTok == 0 {
		return ctx + " ctx"
	}
	return fmt.Sprintf("$%.2f→$%.2f /M · %s ctx",
		float64(p.InputMicrosPerMTok)/1e6,
		float64(p.OutputMicrosPerMTok)/1e6,
		ctx)
}

// compactCount renders big numbers the way a status line wants them:
// 1048576 becomes 1.0M, 128000 becomes 128k.
func compactCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}
