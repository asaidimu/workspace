package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The demo's palette. Truecolor, dark-terminal friendly, and fixed so the
// screenshots and tests do not depend on the host's theme.
var (
	colAccent = lipgloss.Color("#7aa2f7")
	colUser   = lipgloss.Color("#7dcfff")
	colAgent  = lipgloss.Color("#bb9af7")
	colTool   = lipgloss.Color("#e0af68")
	colOK     = lipgloss.Color("#9ece6a")
	colErr    = lipgloss.Color("#f7768e")
	colMuted  = lipgloss.Color("#7f849c")
	colText   = lipgloss.Color("#c0caf5")
	colPanel  = lipgloss.Color("#24283b")
	colCode   = lipgloss.Color("#1f2335")
)

var (
	wordmarkStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	mutedStyle    = lipgloss.NewStyle().Foreground(colMuted)
	textStyle     = lipgloss.NewStyle().Foreground(colText)
	errorStyle    = lipgloss.NewStyle().Foreground(colErr)
	okStyle       = lipgloss.NewStyle().Foreground(colOK)
	accentStyle   = lipgloss.NewStyle().Foreground(colAccent)
	boldStyle     = lipgloss.NewStyle().Bold(true).Foreground(colText)

	headerStyle = lipgloss.NewStyle().
			Background(colPanel).
			Foreground(colText).
			Padding(0, 1)

	composerStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colMuted).
			Padding(0, 1)

	composerActiveStyle = composerStyle.BorderForeground(colAccent)

	pickerPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colMuted).
				Padding(1, 2)

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
		return lipgloss.NewStyle().Bold(true).Foreground(colUser)
	case "agent":
		return lipgloss.NewStyle().Bold(true).Foreground(colAgent)
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
