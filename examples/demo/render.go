package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/asaidimu/workspace/domain"
)

// renderCtx carries what a turn's renderer needs to know about the session:
// the wrap width, whether detail is expanded (ctrl+o), whether a run is in
// flight, and which tool calls already have results.
type renderCtx struct {
	width     int
	expand    bool
	runActive bool
	done      map[string]bool
}

// renderTurn draws one turn in the opencode manner: the user's words are
// demoted to a dim prompt-marked block, the assistant speaks in full-width
// markdown under a small label, and tool traffic renders as status rows.
func renderTurn(t domain.Turn, rc renderCtx) string {
	var lines []string
	switch t.Actor.Kind {
	case domain.ActorUser:
		lines = renderUserTurn(t, rc)
	default:
		lines = renderAgentTurn(t, rc)
	}
	return strings.Join(lines, "\n")
}

// renderUserTurn is the demoted user message: an accent `❯`, then the dim
// text. No gutter, no label — the assistant's output owns the screen.
func renderUserTurn(t domain.Turn, rc renderCtx) []string {
	glyph := accentStyle.Render("❯")
	var lines []string
	for _, b := range t.Blocks {
		var body []string
		switch b.Type {
		case "text":
			body = wrapText(textFrom(b), max(rc.width-2, 10))
		case "image":
			body = []string{mutedStyle.Render("◻ image " + blockString(b, "ref"))}
		case "file":
			body = []string{mutedStyle.Render("◻ file " + blockString(b, "name"))}
		default:
			body = wrapText(turnBody(domain.Turn{Blocks: []domain.Block{b}}), max(rc.width-2, 10))
		}
		for i, l := range body {
			if i == 0 && len(lines) == 0 {
				lines = append(lines, glyph+" "+userStyle.Render(l))
				continue
			}
			lines = append(lines, "  "+userStyle.Render(l))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, glyph+" "+mutedStyle.Render("(empty)"))
	}
	return lines
}

// renderAgentTurn puts a small label above the turn's blocks; the blocks
// themselves span the full width like opencode's assistant output.
func renderAgentTurn(t domain.Turn, rc renderCtx) []string {
	label := roleLabel(t.Actor.Kind)
	body := turnBlocks(t, rc)
	lines := []string{faintStyle.Render("●") + " " + mutedStyle.Render(label)}
	lines = append(lines, body...)
	if len(body) == 0 {
		lines = append(lines, mutedStyle.Render("(empty turn)"))
	}
	return lines
}

// turnBlocks renders a turn's blocks into styled lines, one blank line
// between blocks.
func turnBlocks(t domain.Turn, rc renderCtx) []string {
	var out []string
	appendBlock := func(lines []string) {
		if len(out) > 0 && len(lines) > 0 {
			out = append(out, "")
		}
		out = append(out, lines...)
	}
	for _, b := range t.Blocks {
		switch b.Type {
		case "text":
			appendBlock(renderMarkdown(textFrom(b), rc.width))
		case "document":
			appendBlock(renderDocument(b, rc.width))
		case "json":
			appendBlock(renderCodeBlock("json", strings.Split(jsonText(b), "\n"), rc.width))
		case "thinking":
			appendBlock(renderThinking(textFrom(b), rc))
		case "tool_call":
			appendBlock(renderToolCall(b, rc))
		case "tool_result":
			appendBlock(renderToolResult(b, rc))
		case "image":
			appendBlock([]string{mutedStyle.Render("◻ image " + blockString(b, "ref"))})
		case "file":
			appendBlock([]string{mutedStyle.Render("◻ file " + blockString(b, "name"))})
		case "unknown":
			appendBlock(wrapText(unknownText(b), rc.width))
		default:
			appendBlock(wrapText("["+b.Type+"]", rc.width))
		}
	}
	return out
}

func renderDocument(b domain.Block, width int) []string {
	v := blockObject(b)
	if v == nil {
		return wrapText(textFrom(b), width)
	}
	title, _ := v["title"].(string)
	text, _ := v["text"].(string)
	var out []string
	if title != "" {
		out = append(out, boldStyle.Render(title))
	}
	if text != "" {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, renderMarkdown(text, width)...)
		return out
	}
	if ref, _ := v["ref"].(string); ref != "" {
		out = append(out, mutedStyle.Render("◻ document "+ref))
		return out
	}
	if len(out) == 0 {
		return wrapText(textFrom(b), width)
	}
	return out
}

// renderThinking collapses the model's reasoning to a one-line preview
// unless detail is expanded (ctrl+o).
func renderThinking(text string, rc renderCtx) []string {
	if rc.expand {
		lines := wrapText(text, max(rc.width-2, 8))
		for i, l := range lines {
			prefix := "◌ "
			if i > 0 {
				prefix = "  "
			}
			lines[i] = mutedStyle.Render(prefix + l)
		}
		return lines
	}
	preview := truncate(strings.Join(strings.Fields(text), " "), max(rc.width-20, 8))
	return []string{mutedStyle.Render("◌ thinking ") + faintStyle.Render(preview+"  ·  ctrl+o")}
}

// renderToolCall draws a status row for one call: its state dot, the tool
// name, and a short argument summary. A call without a result under an
// active run is running; one without a result on an idle session never
// completed.
func renderToolCall(b domain.Block, rc renderCtx) []string {
	name := blockString(b, "name")
	callID := blockString(b, "call_id")

	var dotStyle lipgloss.Style
	var state string
	switch {
	case rc.done[callID]:
		dotStyle = lipgloss.NewStyle().Foreground(colTool)
	case rc.runActive:
		dotStyle = accentStyle
		state = " " + accentStyle.Render("running…")
	default:
		dotStyle = errorStyle
		state = " " + errorStyle.Render("no result")
	}

	head := dotStyle.Render("●") + " " + boldStyle.Render(name) + state

	args := toolArguments(b)
	if cmd, ok := args["command"].(string); ok {
		head += " " + codeStyle.Render(truncate("$ "+cmd, max(rc.width-lipgloss.Width(name)-8, 8)))
		return []string{head}
	}
	if len(args) > 0 {
		if raw, err := json.Marshal(args); err == nil {
			head += " " + mutedStyle.Render(truncate(string(raw), max(rc.width-lipgloss.Width(name)-8, 8)))
		}
	}
	return []string{head}
}

// renderToolResult draws what a call produced. Collapsed (the default) it
// is the head line plus a one-line preview and a "+N lines" hint; expanded
// (ctrl+o) it is the full capped output block.
func renderToolResult(b domain.Block, rc renderCtx) []string {
	m := blockObject(b)
	style := mutedStyle
	if m != nil {
		if isErr, _ := m["is_error"].(bool); isErr {
			style = errorStyle
		}
	}

	var stdouts, stderrs []string
	var nOut, nErr int
	exit := ""
	if m != nil {
		if o := asObject(m["output"]); o != nil {
			if code, ok := o["exit_code"]; ok {
				exit = fmt.Sprintf("exit %v", code)
			}
			stdouts, nOut = outputLines(o["stdout"], rc.width, false, rc.expand)
			stderrs, nErr = outputLines(o["stderr"], rc.width, true, rc.expand)
		}
	}

	head := style.Render("↳")
	if exit != "" {
		head += " " + faintStyle.Render(exit)
	}
	lines := []string{head}

	if stdouts == nil && stderrs == nil {
		out := blockString(b, "output")
		if rc.expand {
			return append(lines, wrapText(out, max(rc.width-2, 8))...)
		}
		if out != "" {
			preview := truncate(strings.Join(strings.Fields(out), " "), max(rc.width-8, 8))
			lines = append(lines, "  "+style.Render(preview))
		}
		return lines
	}

	hidden := 0
	if !rc.expand {
		// One preview line from whichever stream has content; the rest is
		// behind the ctrl+o hint, counted from the real totals rather than
		// the capped render.
		all := append(append([]string{}, stdouts...), stderrs...)
		switch {
		case len(all) > 1:
			if len(stdouts) > 0 {
				lines = append(lines, stdouts[0])
			} else {
				lines = append(lines, stderrs[0])
			}
			hidden = nOut + nErr - 1
		case len(all) == 1:
			lines = append(lines, all[0])
		}
		if hidden > 0 {
			lines = append(lines, faintStyle.Render(fmt.Sprintf("  … +%d lines  ·  ctrl+o", hidden)))
		}
		return lines
	}

	lines = append(lines, stdouts...)
	lines = append(lines, stderrs...)
	return lines
}

// outputLines renders a tool stream as an indented code block, capped so a
// chatty command cannot take over the transcript: 2 lines collapsed, 200
// expanded. The second return is the stream's real line count, so the
// collapse hint can say how much is hidden.
func outputLines(value any, width int, isErr, expand bool) ([]string, int) {
	text, _ := value.(string)
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, 0
	}
	raw := strings.Split(text, "\n")
	total := len(raw)
	maxLines := 2
	if expand {
		maxLines = 200
	}
	truncated := len(raw) > maxLines
	if truncated {
		raw = raw[:maxLines]
	}
	var out []string
	style := codeStyle
	if isErr {
		style = lipgloss.NewStyle().Background(colCode).Foreground(colErr)
	}
	for _, l := range raw {
		out = append(out, "  "+style.Render(fillWidth(truncate(l, max(width-4, 8)), max(width-4, 8))))
	}
	if truncated {
		out = append(out, faintStyle.Render("  … output capped"))
	}
	return out, total
}

func renderCodeBlock(lang string, body []string, width int) []string {
	inner := max(width-2, 8)
	var out []string
	if lang != "" {
		out = append(out, mutedStyle.Render("  "+lang))
	}
	for _, line := range body {
		line = strings.ReplaceAll(line, "\t", "    ")
		out = append(out, "  "+codeStyle.Render(fillWidth(truncate(line, inner), inner)))
	}
	if len(body) == 0 {
		out = append(out, "  "+codeStyle.Render(strings.Repeat(" ", inner)))
	}
	return out
}

// asObject normalizes a decoded value into an object: the stores hand back
// map[string]any, but a nested raw JSON field can surface as a jsontext.Value
// or a byte slice, so a JSON round trip is the one safe conversion.
func asObject(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// findToolCall locates a committed tool_call block by call ID and returns
// its tool name and decoded arguments. The agent commits the assistant turn
// before it parks for approval, so the arguments are already in the chain.
func findToolCall(turns []domain.Turn, callID string) (string, map[string]any) {
	for i := len(turns) - 1; i >= 0; i-- {
		for _, b := range turns[i].Blocks {
			if b.Type != "tool_call" || blockString(b, "call_id") != callID {
				continue
			}
			name := blockString(b, "name")
			args := toolArguments(b)
			return name, args
		}
	}
	return "", nil
}

// toolArguments returns a tool call's arguments as an object, decoding the
// raw JSON when the payload keeps them as bytes.
func toolArguments(b domain.Block) map[string]any {
	v := blockObject(b)
	if v == nil {
		return nil
	}
	switch args := v["arguments"].(type) {
	case map[string]any:
		return args
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(args), &m) == nil {
			return m
		}
	}
	return nil
}

func roleLabel(kind domain.ActorKind) string {
	switch kind {
	case domain.ActorUser:
		return "you"
	case domain.ActorAgent:
		return "agent"
	case domain.ActorTool:
		return "tool"
	default:
		return string(kind)
	}
}

// turnBody renders one turn's blocks as plain text: the payload both the
// prompt pipeline and the tests read. Styling lives in renderTurn.
func turnBody(t domain.Turn) string {
	var parts []string
	for _, b := range t.Blocks {
		switch b.Type {
		case "text":
			parts = append(parts, textFrom(b))
		case "document":
			parts = append(parts, documentText(b))
		case "json":
			parts = append(parts, jsonText(b))
		case "thinking":
			parts = append(parts, textFrom(b))
		case "tool_call":
			parts = append(parts, "[tool call "+blockString(b, "name")+"]")
		case "tool_result":
			parts = append(parts, "[tool result "+blockString(b, "output")+"]")
		case "image":
			parts = append(parts, "[image "+blockString(b, "ref")+"]")
		case "file":
			parts = append(parts, "[file "+blockString(b, "name")+"]")
		case "unknown":
			parts = append(parts, unknownText(b))
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	if len(parts) == 0 {
		return "(empty turn)"
	}
	return strings.Join(parts, "\n")
}

// documentText shows a document the way a reader wants it: the title, then
// the inline text. A blob-backed document has no bytes in the turn, so the
// reference stands in for the body.
func documentText(b domain.Block) string {
	v := blockObject(b)
	if v == nil {
		return textFrom(b)
	}
	title, _ := v["title"].(string)
	text, _ := v["text"].(string)
	if text != "" {
		if title == "" {
			return text
		}
		return title + "\n\n" + text
	}
	if ref, _ := v["ref"].(string); ref != "" {
		if title == "" {
			return "[document " + ref + "]"
		}
		return "[" + title + ": " + ref + "]"
	}
	return textFrom(b)
}

// jsonText pretty-prints a json block's payload, which is the block value
// itself rather than a text wrapper.
func jsonText(b domain.Block) string {
	data, err := json.MarshalIndent(b.Value, "", "  ")
	if err != nil {
		return fmt.Sprint(b.Value)
	}
	return string(data)
}

// unknownText renders an uninterpretable block as the text it carried. Raw
// holds the fence body (possibly not valid JSON, hence not b.Value), so it
// wins when present; otherwise the value's preserved "raw" member is used.
func unknownText(b domain.Block) string {
	if s := strings.TrimSpace(string(b.Raw)); s != "" {
		return s
	}
	v := blockObject(b)
	if v == nil {
		return fmt.Sprint(b.Value)
	}
	if raw, ok := v["raw"]; ok {
		if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		if data, err := json.Marshal(raw); err == nil && string(data) != "null" {
			return string(data)
		}
	}
	return fmt.Sprint(b.Value)
}

// textFrom pulls the string out of a text block whatever shape the store
// handed back: the {"text": ...} map, a typed payload, a bare string, or
// raw JSON.
func textFrom(b domain.Block) string {
	v := blockObject(b)
	if v != nil {
		if s, ok := v["text"].(string); ok {
			return s
		}
	}
	switch b.Value.(type) {
	case string:
		return fmt.Sprint(b.Value)
	}
	if len(b.Raw) > 0 {
		var m map[string]any
		if err := json.Unmarshal(b.Raw, &m); err == nil {
			if s, ok := m["text"].(string); ok {
				return s
			}
		}
		var s string
		if err := json.Unmarshal(b.Raw, &s); err == nil {
			return s
		}
	}
	return fmt.Sprint(b.Value)
}

// blockObject renders a block's value (map or typed payload) as an object.
func blockObject(b domain.Block) map[string]any {
	if m, ok := b.Value.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(b.Value)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func blockString(b domain.Block, key string) string {
	m := blockObject(b)
	if m == nil {
		return "?"
	}
	return fmt.Sprint(m[key])
}
