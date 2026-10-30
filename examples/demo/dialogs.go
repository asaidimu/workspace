package main

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asaidimu/workspace/store"
)

// dialogRow is one selectable line in an overlay dialog.
type dialogRow struct {
	primary   string
	secondary string
	modelID   string
	convID    string
}

// openDialog switches to an overlay and resets its cursor and filter.
func (a *app) openDialog(name string) {
	a.dialog = name
	a.dialogCursor = 0
	a.dialogFilter = ""
	if name == dialogSessions {
		a.refreshSessions()
	}
}

func (a *app) closeDialog() {
	a.dialog = dialogNone
	a.dialogCursor = 0
	a.dialogFilter = ""
}

// refreshSessions re-reads the conversation list for the sessions dialog,
// newest first.
func (a *app) refreshSessions() {
	page, err := a.cfg.handle.Conversations().List(context.Background(),
		store.ConversationFilter{}, store.PageRequest{Limit: 50})
	if err != nil {
		a.notice = fmt.Sprintf("list conversations: %v", err)
		a.cfg.sessions = nil
		return
	}
	a.cfg.sessions = page.Items
}

// dialogKeys routes input while an overlay is open: move, filter, select,
// close. Typing filters model IDs and session titles.
func (a *app) dialogKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		a.closeDialog()
		return a, nil
	case "up", "ctrl+k":
		a.dialogCursor--
	case "down", "ctrl+j":
		a.dialogCursor++
	case "enter":
		return a.dialogEnter()
	case "backspace":
		r := []rune(a.dialogFilter)
		if len(r) > 0 {
			a.dialogFilter = string(r[:len(r)-1])
		}
		return a, nil
	default:
		if len(msg.Runes) > 0 {
			a.dialogFilter += string(msg.Runes)
			a.dialogCursor = 0
		}
		return a, nil
	}
	rows := a.dialogMatches()
	n := max(len(rows), 1)
	a.dialogCursor = ((a.dialogCursor % n) + n) % n
	return a, nil
}

// dialogMatches applies the dialog's filter to its source list.
func (a *app) dialogMatches() []dialogRow {
	filter := strings.ToLower(a.dialogFilter)
	var rows []dialogRow
	switch a.dialog {
	case dialogModel:
		for _, m := range a.cfg.models {
			if filter != "" && !strings.Contains(strings.ToLower(m.ID), filter) {
				continue
			}
			rows = append(rows, dialogRow{
				primary:   m.ID,
				secondary: describePricing(m),
				modelID:   m.ID,
			})
		}
	case dialogSessions:
		for _, c := range a.cfg.sessions {
			if filter != "" && !strings.Contains(strings.ToLower(c.Title), filter) {
				continue
			}
			rows = append(rows, dialogRow{
				primary:   c.Title,
				secondary: c.UpdatedAt.Format("Jan 2 15:04"),
				convID:    c.ID,
			})
		}
	case dialogHelp:
		rows = []dialogRow{{primary: "help"}}
	}
	return rows
}

// dialogEnter acts on the highlighted row: switch model, switch conversation,
// or close help.
func (a *app) dialogEnter() (tea.Model, tea.Cmd) {
	rows := a.dialogMatches()
	if a.dialog == dialogHelp {
		a.closeDialog()
		return a, nil
	}
	if len(rows) == 0 {
		return a, nil
	}
	row := rows[min(a.dialogCursor, len(rows)-1)]
	switch a.dialog {
	case dialogModel:
		if row.modelID != "" && a.runID == "" {
			a.modelID = row.modelID
			a.closeDialog()
			a.status = "ready"
		}
	case dialogSessions:
		if row.convID != "" && a.runID == "" {
			a.convID = row.convID
			a.pending = nil
			a.queued = nil
			a.failure = ""
			a.closeDialog()
			a.reload()
		}
	}
	return a, nil
}

// dialogView draws the overlay box, or "" when none is open.
func (a *app) dialogView() string {
	switch a.dialog {
	case dialogModel:
		return a.listDialog("Select model", "model ")
	case dialogSessions:
		return a.listDialog("Sessions", "")
	case dialogHelp:
		return a.helpDialog()
	}
	return ""
}

// listWindow is how many rows the picker and the list dialogs render before
// they scroll. OpenRouter's live catalogue alone runs to hundreds of models;
// showing them all would overflow any terminal.
const listWindow = 12

// windowAround returns the [lo,hi) slice of n rows that keeps cursor visible,
// the way a text editor's viewport scrolls.
func windowAround(cursor, n, height int) (int, int) {
	if n <= height {
		return 0, n
	}
	lo := max(0, min(cursor-height/2, n-height))
	return lo, lo + height
}

// listDialog is the shared shape of the model and sessions dialogs: a
// filtered list with a highlighted row, an optional filter line and a hint.
// Long lists scroll around the cursor with a position counter.
func (a *app) listDialog(title, filterPrefix string) string {
	rows := a.dialogMatches()
	boxWidth := min(max(a.width-10, 30), 64)

	var b strings.Builder
	b.WriteString(boldStyle.Render(title))
	if a.dialogFilter != "" {
		b.WriteString("  " + accentStyle.Render(filterPrefix+a.dialogFilter))
	}
	if len(rows) > listWindow {
		b.WriteString("  " + mutedStyle.Render(fmt.Sprintf("%d/%d", a.dialogCursor+1, len(rows))))
	}
	b.WriteString("\n")

	lo, hi := windowAround(a.dialogCursor, len(rows), listWindow)
	if len(rows) == 0 {
		b.WriteString(mutedStyle.Render("  no matches") + "\n")
	}
	if lo > 0 {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ↑ %d more", lo)) + "\n")
	}
	for i := lo; i < hi; i++ {
		r := rows[i]
		row := "  " + truncate(r.primary, boxWidth-20) + "  " + r.secondary
		if i == a.dialogCursor {
			b.WriteString(paletteSelectedStyle.Width(boxWidth).Render(truncate(row, boxWidth)) + "\n")
			continue
		}
		b.WriteString(fillWidth(truncate(row, boxWidth), boxWidth) + "\n")
	}
	if hi < len(rows) {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ↓ %d more", len(rows)-hi)) + "\n")
	}
	b.WriteString(mutedStyle.Render("↑/↓ move · enter select · esc close · type to filter"))
	return dialogStyle.Width(boxWidth).Render(strings.TrimRight(b.String(), "\n"))
}

// helpDialog is what /help always should have been: the command list and
// the keymap, in one box.
func (a *app) helpDialog() string {
	boxWidth := min(max(a.width-10, 40), 72)

	row := func(k, v string) string {
		return "  " + accentStyle.Render(k) + " " + mutedStyle.Render(v)
	}
	var b strings.Builder
	b.WriteString(boldStyle.Render("Commands") + "\n")
	b.WriteString(row("/help", "this panel") + "\n")
	b.WriteString(row("/new", "start a new conversation") + "\n")
	b.WriteString(row("/model", "switch model") + "\n")
	b.WriteString(row("/sessions", "switch conversation") + "\n")
	b.WriteString(row("/retry", "retry the last failed run") + "\n")
	b.WriteString(row("/quit", "leave the demo") + "\n")
	b.WriteString("\n" + boldStyle.Render("Keys") + "\n")
	b.WriteString(row("enter", "send · queues while a run is in flight") + "\n")
	b.WriteString(row("ctrl+j", "newline in the composer") + "\n")
	b.WriteString(row("ctrl+o", "expand tool output and thinking") + "\n")
	b.WriteString(row("ctrl+r", "retry after a failure") + "\n")
	b.WriteString(row("esc", "cancel the run, or quit") + "\n")
	b.WriteString(row("pgup/pgdn, ctrl+u/ctrl+d, wheel", "scroll") + "\n")
	b.WriteString(mutedStyle.Render("esc closes"))
	return dialogStyle.Width(boxWidth).Render(strings.TrimRight(b.String(), "\n"))
}

// overlayCenter composites the dialog box over the chat view: whole lines
// of the base are replaced, so the box's own background makes it opaque and
// no ANSI slicing is needed. The box sits about a third down the screen.
func overlayCenter(base, box string, width, height int) string {
	baseLines := strings.Split(base, "\n")
	boxLines := strings.Split(box, "\n")
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}

	top := (height - len(boxLines)) / 3
	if top < 1 {
		top = 1
	}
	if top+len(boxLines) > height {
		top = max(height-len(boxLines), 0)
	}
	for i, bl := range boxLines {
		y := top + i
		if y >= len(baseLines) {
			break
		}
		pad := (width - lipgloss.Width(bl)) / 2
		if pad < 0 {
			pad = 0
		}
		baseLines[y] = strings.Repeat(" ", pad) + bl
	}
	return strings.Join(baseLines, "\n")
}
