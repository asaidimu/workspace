package main

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/muesli/termenv"
)

// Markdown rendering for the demo: glamour with a style re-tuned to the
// opencode neutral look. Glamour is too heavy to run on every keystroke, so
// renders are cached by content hash and width, with one renderer per width.

const (
	cacheLimit   = 500
	minWrapWidth = 12
)

// neutralStyle retunes glamour's dark style: amber headings, a quiet
// chroma theme, tight margins, no loud H1 banner.
func neutralStyle() ansi.StyleConfig {
	sc := styles.DarkStyleConfig

	sc.Document.StylePrimitive.BlockPrefix = ""
	sc.Document.StylePrimitive.BlockSuffix = ""
	sc.Document.StylePrimitive.Color = strPtr("#d6d6d6")
	sc.Document.Margin = uintPtr(0)

	sc.Heading.StylePrimitive.Color = strPtr("#e8a04c")
	sc.Heading.StylePrimitive.Bold = boolPtr(true)
	sc.H1 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Color: strPtr("#e8a04c"), Bold: boolPtr(true),
	}}
	sc.H2 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Color: strPtr("#e8a04c"), Bold: boolPtr(true),
	}}
	sc.H3 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Color: strPtr("#c8c8c8"), Bold: boolPtr(true),
	}}
	sc.H4 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Color: strPtr("#c8c8c8"), Bold: boolPtr(true),
	}}
	sc.H5 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Color: strPtr("#9a9a9a"), Bold: boolPtr(true),
	}}
	sc.H6 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Color: strPtr("#9a9a9a"), Bold: boolPtr(true),
	}}

	sc.Paragraph = ansi.StyleBlock{}

	sc.List.LevelIndent = 2
	sc.Item = ansi.StylePrimitive{BlockPrefix: "• "}
	sc.Enumeration = ansi.StylePrimitive{BlockPrefix: ". "}

	sc.BlockQuote.StylePrimitive.Color = strPtr("#666666")
	sc.BlockQuote.IndentToken = strPtr("│ ")

	sc.Code.StylePrimitive.Color = strPtr("#e8a04c")
	sc.Code.StylePrimitive.BlockPrefix = ""
	sc.Code.StylePrimitive.BlockSuffix = ""
	sc.CodeBlock.Theme = "nord"

	sc.Link.Color = strPtr("#e8a04c")
	sc.Link.Underline = boolPtr(true)

	// Dark's copy re-emits markdown markers (", **, ~~); the neutral look
	// relies on colour and weight instead.
	sc.Strong = ansi.StylePrimitive{Bold: boolPtr(true), Color: strPtr("#f0f0f0")}
	sc.Emph = ansi.StylePrimitive{Italic: boolPtr(true)}
	sc.Strikethrough = ansi.StylePrimitive{CrossedOut: boolPtr(true)}

	sc.HorizontalRule.Color = strPtr("#3a3a3a")
	sc.HorizontalRule.Format = "\n────────\n"

	return sc
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func uintPtr(u uint) *uint    { return &u }

// glamRenderers holds one word-wrapped renderer per width; glamCache holds
// finished renders keyed by content hash and width. The profile is fixed
// once: a real terminal gets its color profile, pipes and tests get plain
// ASCII (glamour's own default is TrueColor even on a dumb pipe, which
// sprays escape bytes into the transcript).
var (
	glamProfile   = termenv.ColorProfile()
	glamRenderers = map[int]*glamour.TermRenderer{}
	glamCache     = map[string]string{}
)

// renderMarkdown renders markdown text to styled lines at the given width,
// through the cache. When glamour cannot render (an obscure profile, a
// chroma misconfiguration) the text degrades to plain wrapped lines, so a
// rendering failure never loses content.
func renderMarkdown(text string, width int) []string {
	width = max(min(width, 400), minWrapWidth)
	key := fmt.Sprintf("%d:%d:%d", fnv32a(text), len(text), width)
	if s, ok := glamCache[key]; ok {
		return strings.Split(s, "\n")
	}

	out, err := glamRender(text, width)
	if err == nil {
		if len(glamCache) >= cacheLimit {
			glamCache = map[string]string{}
		}
		out = cleanANSI(out)
		glamCache[key] = out
		return strings.Split(out, "\n")
	}

	// Degrade to the old hand-rolled path rather than dropping the block.
	var b []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		b = append(b, wrapText(line, width)...)
	}
	return b
}

// glamRender runs the text through a per-width glamour renderer. The
// renderer picks its color profile from the terminal, so tests (no TTY)
// exercise the wrap logic with plain text.
func glamRender(text string, width int) (string, error) {
	r, ok := glamRenderers[width]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(
			glamour.WithColorProfile(glamProfile),
			glamour.WithStyles(neutralStyle()),
			glamour.WithWordWrap(width),
		)
		if err != nil {
			return "", err
		}
		glamRenderers[width] = r
	}
	out, err := r.Render(text)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// fnv32a is the cache's content hash; collisions also carry the length, and
// a collision would only cost a wrong style, never lost text.
func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// cleanANSI makes glamour's output safe for the profile that rendered it:
// on a color profile only the broken non-CSI escapes are dropped, while a
// colorless profile (pipes, dumb terminals, tests) gets all ANSI stripped —
// glamour's own option does not reach chroma's formatter, which would
// otherwise sprinkle invisible sequences between code tokens.
func cleanANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	if glamProfile == termenv.Ascii {
		return stripANSI(s)
	}
	return stripBrokenEscapes(s)
}

// stripBrokenEscapes drops ESC bytes that do not open a real CSI sequence.
func stripBrokenEscapes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && (i+1 >= len(s) || s[i+1] != '[') {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// stripANSI removes every CSI escape sequence: ESC, then parameter and
// intermediate bytes, then the final byte that ends the sequence.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			if i < len(s) {
				i++ // the final byte
			}
			continue
		}
		i++ // a lone ESC
	}
	return b.String()
}
