package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// command is one slash command. args is whatever followed the name; the
// current commands ignore it, but the parser keeps it so arguments can be
// added without changing the dispatch shape.
type command struct {
	name string
	desc string
	run  func(a *app, args string) (tea.Model, tea.Cmd)
}

func commandTable() []command {
	return []command{
		{name: "/help", desc: "show the command list", run: runHelp},
		{name: "/new", desc: "start a new conversation", run: runNew},
		{name: "/model", desc: "choose a different model", run: runModel},
		{name: "/retry", desc: "retry the last failed run", run: runRetry},
		{name: "/quit", desc: "leave the demo", run: runQuit},
	}
}

// paletteVisible reports whether the composer is in command-entry mode: a
// leading slash with no space yet.
func (a *app) paletteVisible() bool {
	if len(a.pending) > 0 {
		return false
	}
	text := a.input.Value()
	return strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \n")
}

// paletteMatches filters the command table by the text after the slash.
func (a *app) paletteMatches() []command {
	text := strings.TrimPrefix(a.input.Value(), "/")
	out := make([]command, 0, len(commandTable()))
	for _, c := range commandTable() {
		if strings.HasPrefix(strings.TrimPrefix(c.name, "/"), text) {
			out = append(out, c)
		}
	}
	return out
}

// paletteView draws the suggestion list above the composer. The selected row
// is highlighted; the list is empty when nothing matches, in which case the
// palette collapses and enter reports the unknown command.
func (a *app) paletteView() string {
	if !a.paletteVisible() {
		return ""
	}
	matches := a.paletteMatches()
	if len(matches) == 0 {
		return ""
	}
	width := max(a.width-6, 16)
	lines := make([]string, 0, len(matches))
	for i, c := range matches {
		row := fmt.Sprintf("  %-8s %s", c.name, c.desc)
		if i == a.cmdCursor {
			row = paletteSelectedStyle.Width(width).Render(row)
		} else {
			row = fillWidth(row, width)
		}
		lines = append(lines, row)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colMuted).
		Padding(0, 1).
		Width(max(a.width-4, 16)).
		Render(strings.Join(lines, "\n"))
}

// runCommand dispatches the composer's text. An exact command name wins;
// otherwise the highlighted suggestion runs with the same arguments.
func (a *app) runCommand(text string) (tea.Model, tea.Cmd) {
	name, args, _ := strings.Cut(text, " ")
	if c, ok := lookupCommand(name); ok {
		a.input.SetValue("")
		a.cmdCursor = 0
		a.syncLayout()
		return c.run(a, args)
	}
	if matches := a.paletteMatches(); len(matches) > 0 {
		c := matches[min(a.cmdCursor, len(matches)-1)]
		a.input.SetValue("")
		a.cmdCursor = 0
		a.syncLayout()
		return c.run(a, strings.TrimSpace(args))
	}
	a.notice = "unknown command: " + name + " (try /help)"
	a.input.SetValue("")
	a.cmdCursor = 0
	a.syncLayout()
	return a, nil
}

func lookupCommand(name string) (command, bool) {
	for _, c := range commandTable() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func runHelp(a *app, _ string) (tea.Model, tea.Cmd) {
	// The palette is the help: show every command at once.
	a.input.SetValue("/")
	a.cmdCursor = 0
	a.syncLayout()
	return a, nil
}

func runNew(a *app, _ string) (tea.Model, tea.Cmd) {
	if a.runID != "" {
		a.notice = "cancel the run first (esc)"
		return a, nil
	}
	conv, err := a.createConversation()
	if err != nil {
		a.notice = fmt.Sprintf("create conversation: %v", err)
		return a, nil
	}
	a.convID = conv.ID
	a.transcript = nil
	a.pending = nil
	a.failure = ""
	a.notice = ""
	a.status = "ready"
	a.input.SetValue("")
	a.syncLayout()
	a.renderTranscript()
	return a, nil
}

func runModel(a *app, _ string) (tea.Model, tea.Cmd) {
	if a.runID != "" {
		a.notice = "cancel the run first (esc)"
		return a, nil
	}
	for i, m := range a.cfg.models {
		if m.ID == a.modelID {
			a.cursor = i
			break
		}
	}
	a.phase = phaseModels
	a.status = "choose a model"
	a.pending = nil
	a.input.SetValue("")
	a.syncLayout()
	return a, nil
}

func runRetry(a *app, _ string) (tea.Model, tea.Cmd) {
	if a.failure == "" {
		a.notice = "nothing to retry"
		return a, nil
	}
	return a.retry()
}

func runQuit(a *app, _ string) (tea.Model, tea.Cmd) {
	a.quitting = true
	return a, tea.Quit
}
