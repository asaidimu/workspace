package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/asaidimu/workspace/domain"
)

// renderTurn draws one turn as a left-gutter message: a coloured bar, a role
// label, then the block bodies. The bar runs the full height of the message
// so a multi-line reply reads as one unit.
func renderTurn(t domain.Turn, width int) string {
	label := roleLabel(t.Actor.Kind)
	style := roleStyle(label)
	bar := style.Render("▌")

	body := turnBlocks(t, max(width-2, 10))
	lines := []string{bar + " " + style.Render(label)}
	for _, l := range body {
		if l == "" {
			lines = append(lines, bar)
			continue
		}
		lines = append(lines, bar+" "+l)
	}
	return strings.Join(lines, "\n")
}

// turnBlocks renders a turn's blocks into styled lines. Each block keeps its
// own shape: prose goes through the markdown renderer, documents show their
// title, JSON becomes a code block, and tool traffic is a one-line summary.
func turnBlocks(t domain.Turn, width int) []string {
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
			appendBlock(renderMarkdown(textFrom(b), width))
		case "document":
			appendBlock(renderDocument(b, width))
		case "json":
			appendBlock(renderCodeBlock("json", strings.Split(jsonText(b), "\n"), width))
		case "thinking":
			appendBlock(renderThinking(textFrom(b), width))
		case "tool_call":
			appendBlock(renderToolCall(b, width))
		case "tool_result":
			appendBlock(renderToolResult(b, width))
		case "image":
			appendBlock([]string{mutedStyle.Render("◻ image " + blockString(b, "ref"))})
		case "file":
			appendBlock([]string{mutedStyle.Render("◻ file " + blockString(b, "name"))})
		case "unknown":
			appendBlock(wrapText(unknownText(b), width))
		default:
			appendBlock(wrapText("["+b.Type+"]", width))
		}
	}
	if len(out) == 0 {
		return []string{mutedStyle.Render("(empty turn)")}
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

func renderThinking(text string, width int) []string {
	lines := wrapText(text, max(width-2, 8))
	for i, l := range lines {
		prefix := "◌ "
		if i > 0 {
			prefix = "  "
		}
		lines[i] = mutedStyle.Render(prefix + l)
	}
	return lines
}

func renderToolCall(b domain.Block, width int) []string {
	name := blockString(b, "name")
	head := lipgloss.NewStyle().Foreground(colTool).Render("⚙ ") + boldStyle.Render(name)
	args := toolArguments(b)
	if cmd, ok := args["command"].(string); ok {
		lines := wrapText("$ "+cmd, max(width-2, 8))
		for i, l := range lines {
			if i == 0 {
				lines[i] = head + " " + codeStyle.Render(l)
			} else {
				lines[i] = "  " + codeStyle.Render(l)
			}
		}
		return lines
	}
	if len(args) > 0 {
		if raw, err := json.Marshal(args); err == nil {
			head += " " + mutedStyle.Render(truncate(string(raw), max(width-lipgloss.Width(name)-3, 8)))
		}
	}
	return []string{head}
}

func renderToolResult(b domain.Block, width int) []string {
	m := blockObject(b)
	out := blockString(b, "output")
	style := mutedStyle
	if m != nil {
		if isErr, _ := m["is_error"].(bool); isErr {
			style = errorStyle
		}
	}
	if m != nil {
		if o := asObject(m["output"]); o != nil {
			if cmd, ok := o["command"].(string); ok {
				head := style.Render("↳ $ " + cmd)
				if code, ok := o["exit_code"]; ok {
					head += " " + mutedStyle.Render(fmt.Sprintf("exit %v", code))
				}
				lines := []string{head}
				lines = append(lines, outputLines(o["stdout"], width, false)...)
				lines = append(lines, outputLines(o["stderr"], width, true)...)
				return lines
			}
		}
	}
	return []string{style.Render("↳ " + truncate(out, max(width-2, 8)))}
}

// outputLines renders a tool stream as an indented code block, capped so a
// chatty command cannot take over the transcript.
func outputLines(value any, width int, isErr bool) []string {
	text, _ := value.(string)
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	raw := strings.Split(text, "\n")
	const maxLines = 12
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
		out = append(out, mutedStyle.Render("  … output truncated"))
	}
	return out
}

// renderMarkdown is a deliberately small markdown renderer: headings,
// bullets, blockquotes, rules, fenced code and inline `code`/`**bold**`.
// It is not a full CommonMark implementation, and does not try to be.
func renderMarkdown(text string, width int) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			var body []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				body = append(body, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // closing fence
			}
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, renderCodeBlock(lang, body, width)...)
			out = append(out, "")
			continue
		}

		switch {
		case trimmed == "":
			out = append(out, "")
		case isRule(trimmed):
			out = append(out, mutedStyle.Render(strings.Repeat("─", max(width, 3))))
		case headingLevel(trimmed) > 0:
			level := headingLevel(trimmed)
			body := strings.TrimSpace(trimmed[level:])
			for _, l := range wrapText(body, width) {
				out = append(out, headingStyle.Render(l))
			}
		case strings.HasPrefix(trimmed, "> "):
			for _, l := range wrapText(strings.TrimPrefix(trimmed, "> "), max(width-2, 8)) {
				out = append(out, quoteStyle.Render("│ "+l))
			}
		case isBullet(trimmed):
			body := bulletBody(trimmed)
			for j, l := range wrapText(body, max(width-2, 8)) {
				prefix := "• "
				if j > 0 {
					prefix = "  "
				}
				out = append(out, accentStyle.Render(prefix)+styleInline(l))
			}
		default:
			for _, l := range wrapText(trimmed, width) {
				out = append(out, styleInline(l))
			}
		}
		i++
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
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

func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

func isRule(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}

func isBullet(s string) bool {
	return strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") || strings.HasPrefix(s, "• ")
}

func bulletBody(s string) string {
	for _, marker := range []string{"- ", "* ", "• "} {
		if strings.HasPrefix(s, marker) {
			return strings.TrimSpace(strings.TrimPrefix(s, marker))
		}
	}
	return s
}

// styleInline applies the two inline spans the demo cares about: `code` and
// **bold**. Unclosed markers are left alone.
func styleInline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '`' {
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteString(inlineCodeStyle.Render(s[i+1 : i+1+j]))
				i += j + 2
				continue
			}
		}
		if strings.HasPrefix(s[i:], "**") {
			if j := strings.Index(s[i+2:], "**"); j >= 0 {
				b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colText).Render(s[i+2 : i+2+j]))
				i += j + 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
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
