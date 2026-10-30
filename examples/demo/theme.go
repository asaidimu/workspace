package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The demo's palette: the opencode neutral look. A near-black surface, dim
// gray body text, and one warm amber accent that marks the interactive and
// in-flight elements. Fixed so the screenshots and tests do not depend on
// the host's theme.
var (
	colAccent = lipgloss.Color("#e8a04c") // amber: focus, in-flight, user glyph
	colText   = lipgloss.Color("#d6d6d6") // assistant body
	colUser   = lipgloss.Color("#9a9a9a") // demoted user text
	colTool   = lipgloss.Color("#8a8a8a") // tool rows
	colOK     = lipgloss.Color("#84af73") // muted green: ready state
	colErr    = lipgloss.Color("#e0685c") // muted red: failures, rejected
	colMuted  = lipgloss.Color("#666666") // hints, labels, meta
	colFaint  = lipgloss.Color("#3a3a3a") // borders, rules, dimmest chrome
	colPanel  = lipgloss.Color("#1b1b1b") // header, selected rows, dialogs
	colCode   = lipgloss.Color("#151515") // code and tool output background
)

var (
	wordmarkStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	mutedStyle    = lipgloss.NewStyle().Foreground(colMuted)
	faintStyle    = lipgloss.NewStyle().Foreground(colFaint)
	textStyle     = lipgloss.NewStyle().Foreground(colText)
	userStyle     = lipgloss.NewStyle().Foreground(colUser)
	errorStyle    = lipgloss.NewStyle().Foreground(colErr)
	okStyle       = lipgloss.NewStyle().Foreground(colOK)
	accentStyle   = lipgloss.NewStyle().Foreground(colAccent)
	boldStyle     = lipgloss.NewStyle().Bold(true).Foreground(colText)

	headerStyle = lipgloss.NewStyle().
			Background(colPanel).
			Foreground(colMuted).
			Padding(0, 1)

	composerStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colFaint).
			Padding(0, 1)

	composerActiveStyle = composerStyle.BorderForeground(colAccent)

	pickerPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colFaint).
				Padding(1, 2)

	dialogStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colAccent).
			Background(colPanel).
			Padding(0, 1)

	codeStyle = lipgloss.NewStyle().
			Background(colCode).
			Foreground(colText)

	inlineCodeStyle = lipgloss.NewStyle().Foreground(colAccent)

	paletteSelectedStyle = lipgloss.NewStyle().
				Background(colPanel).
				Foreground(colAccent).
				Bold(true)
	quoteStyle   = lipgloss.NewStyle().Foreground(colMuted)
	headingStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
)

// roleStyle is the label colour for one actor kind.
func roleStyle(label string) lipgloss.Style {
	switch label {
	case "you":
		return lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	case "agent":
		return lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	case "tool":
		return lipgloss.NewStyle().Bold(true).Foreground(colTool)
	default:
		return lipgloss.NewStyle().Bold(true).Foreground(colMuted)
	}
}

// padBetween places left and right at the edges of a width-wide line.
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left + " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

// fillWidth right-pads s with spaces to width, ignoring ANSI escapes.
func fillWidth(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// wrapText greedily wraps words to width columns, hard-splitting words that
// are longer than a line. It ignores existing line breaks; callers split
// lines first when that matters.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := ""
	for _, w := range words {
		for lipgloss.Width(w) > width {
			head, tail := hardSplit(w, width)
			if line != "" {
				out = append(out, line)
				line = ""
			}
			out = append(out, head)
			w = tail
		}
		switch {
		case line == "":
			line = w
		case lipgloss.Width(line)+1+lipgloss.Width(w) <= width:
			line += " " + w
		default:
			out = append(out, line)
			line = w
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// hardSplit cuts a word at the widest rune boundary that fits.
func hardSplit(s string, width int) (string, string) {
	var head []rune
	for _, r := range s {
		if lipgloss.Width(string(head)+string(r)) > width {
			break
		}
		head = append(head, r)
	}
	if len(head) == 0 {
		// A single rune wider than the line (a CJK glyph, an emoji) still
		// has to advance, or the caller's loop would never terminate.
		for _, r := range s {
			head = append(head, r)
			break
		}
	}
	return string(head), s[len(string(head)):]
}

// truncate shortens s to width columns, marking the cut with an ellipsis.
func truncate(s string, width int) string {
	if width <= 1 || lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	var out []rune
	for _, r := range runes {
		if lipgloss.Width(string(out)+string(r)) > width-1 {
			break
		}
		out = append(out, r)
	}
	return string(out) + "…"
}
