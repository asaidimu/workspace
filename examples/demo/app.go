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

// phase is which screen the program is on.
type phase int

const (
	phaseModels phase = iota
	phaseChat
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

	case eventMsg:
		line := fmt.Sprintf("%s %s", msg.env.At.Format("15:04:05"), msg.env.Name)
		a.lastEvent = line
		a.events = append(a.events, line)
		if len(a.events) > 200 {
			a.events = a.events[len(a.events)-200:]
		}
		if req, ok := msg.env.Event.(agent.ApprovalRequested); ok {
			a.reload()
			a.addApproval(req)
		}
		return a, nil

	case pollRunMsg:
		return a.poll(msg.runID)

	case tea.KeyMsg:
		return a.key(msg)
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
		if a.runID != "" {
			return a, nil // a run is in flight; wait for it
		}
		text := strings.TrimSpace(a.input.Value())
		if text == "" {
			return a, nil
		}
		if strings.HasPrefix(text, "/") {
			return a.runCommand(text)
		}
		return a.send(text)
	case "ctrl+j":
		a.input.InsertString("\n")
		a.syncLayout()
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
// sends into, so the first message needs no setup.
func (a *app) selectModel() tea.Cmd {
	m := a.cfg.models[a.cursor]
	a.modelID = m.ID

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
// agent turns and tool traffic — oldest first.
func (a *app) reload() {
	chain, err := a.cfg.handle.Turns().Chain(context.Background(), a.convID, store.ChainOptions{})
	if err != nil {
		a.notice = fmt.Sprintf("read chain: %v", err)
		return
	}
	a.transcript = chain
	a.renderTranscript()
}

// syncLayout sizes the transcript and composer for the current window and
// input height. The composer grows with the text (up to six rows) and the
// transcript gives back the rows it takes.
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
	a.view.Height = max(a.height-a.composerHeight()-a.paletteHeight()-4, 3)
	a.renderTranscript()
}

// renderTranscript rebuilds the viewport's content from the chain. It keeps
// the scroll position when the user has scrolled away from the bottom, and
// pins to the newest message otherwise.
func (a *app) renderTranscript() {
	atBottom := a.view.AtBottom()
	width := max(a.width-2, 20)

	var b strings.Builder
	for i, t := range a.transcript {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(renderTurn(t, width))
	}
	if a.runID != "" && len(a.pending) == 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(roleStyle("agent").Render("▌") + " " + a.spin.View() + " " +
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
	return a.chatView()
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
// error that needs attention.
func (a *app) footerView() string {
	status, hints := a.statusLine()
	first := padBetween(status, mutedStyle.Render(hints), max(a.width, 20))

	second := mutedStyle.Render("event: " + orNone(a.lastEvent))
	switch {
	case a.failure != "":
		second = errorStyle.Render("✗ " + a.failure)
	case a.notice != "":
		second = errorStyle.Render("✗ " + a.notice)
	}
	second = padBetween(second, "", max(a.width, 20))
	return first + "\n" + second
}

func (a *app) statusLine() (status, hints string) {
	if len(a.pending) > 0 {
		return errorStyle.Render("◌ waiting for permission"), "y approve · n reject · esc cancel"
	}
	if a.runID != "" {
		return a.spin.View() + " " + textStyle.Render("working"), "esc cancel · pgup/pgdn scroll"
	}
	if a.failure != "" || a.status == "failed" {
		return errorStyle.Render("✗ failed"), "ctrl+r retry · esc quit"
	}
	return okStyle.Render("● ready"), "enter send · / commands · ctrl+j newline · esc quit"
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
	parts = append(parts, a.composerView(), a.footerView())
	return strings.Join(parts, "\n")
}

func (a *app) modelsView() string {
	panelWidth := min(max(a.width-12, 24), 64)
	var b strings.Builder
	b.WriteString(wordmarkStyle.Render("workspace") + " " + mutedStyle.Render("demo") + "\n")
	b.WriteString(mutedStyle.Render("choose a model") + "\n\n")

	for i, m := range a.cfg.models {
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
	p := m.Pricing
	if p.InputMicrosPerMTok == 0 && p.OutputMicrosPerMTok == 0 {
		return fmt.Sprintf("%d ctx", m.Capabilities.ContextWindow)
	}
	return fmt.Sprintf("$%.2f/M in · $%.2f/M out · %d ctx",
		float64(p.InputMicrosPerMTok)/1e6,
		float64(p.OutputMicrosPerMTok)/1e6,
		m.Capabilities.ContextWindow)
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}
