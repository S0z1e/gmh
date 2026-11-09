package tui

import (
	"strings"
)

// formatMarkdown renders the small markdown subset the TUI shows:
// **bold**, `code`, "# "/"## " headings, [text](url), "> quote",
// "*" / "-" list markers, and fenced code blocks. Everything else
// stays base text. It is stdlib only (no glamour) and safe to run on
// partial streamed text: an unclosed marker renders literally, and
// text with no markdown comes back byte-identical.
//
// renderText applies the same formatting and then indents every line
// 3 spaces — opencode's gutter, which the assistant text, tool rows
// and usage lines all sit under.
func renderText(s string) string {
	lines := strings.Split(formatMarkdown(s), "\n")
	for i, l := range lines {
		lines[i] = "   " + l
	}
	return strings.Join(lines, "\n")
}

func formatMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	inFence := false
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue // fence markers are not shown
		}
		if inFence {
			// Fenced content: green on raised, indented 3 + 2
			// (the extra 2 are added here; renderText adds 3).
			out = append(out, styleCodeBlk.Render("  "+l))
			continue
		}
		out = append(out, formatMarkdownLine(l))
	}
	return strings.Join(out, "\n")
}

func formatMarkdownLine(l string) string {
	// Headings: "# " / "## " / "### " → purple bold, markers dropped.
	if strings.HasPrefix(l, "# ") || strings.HasPrefix(l, "## ") || strings.HasPrefix(l, "### ") {
		text := strings.TrimLeft(l, "# ")
		return stylePurple.Render(formatInline(text))
	}
	// Blockquote: "> " → quote colour, marker dropped.
	if strings.HasPrefix(l, "> ") {
		return styleQuote.Render(formatInline(strings.TrimPrefix(l, "> ")))
	}
	// List markers: "- " / "* " → peach marker, rest formatted.
	if strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* ") {
		return stylePeach.Render(l[:1]) + " " + formatInline(l[2:])
	}
	return formatInline(l)
}

// formatInline renders **bold**, `code` and [text](url) inside one
// line. The URL is dropped in the TUI (it stays in the JSONL
// session). Markers without a closing partner stay literal, so
// partial streamed text never renders half-styled.
func formatInline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j >= 0 {
				b.WriteString(styleBold.Render(s[i+2 : i+2+j]))
				i += 2 + j + 2
			} else {
				b.WriteByte(s[i])
				i++
			}
		case s[i] == '`':
			if j := strings.Index(s[i+1:], "`"); j >= 0 {
				b.WriteString(styleCode.Render(s[i+1 : i+1+j]))
				i += 1 + j + 1
			} else {
				b.WriteByte(s[i])
				i++
			}
		case s[i] == '[':
			if j := strings.Index(s[i:], "]("); j >= 0 {
				if k := strings.Index(s[i+j+2:], ")"); k >= 0 {
					b.WriteString(stylePeach.Render(s[i+1 : i+j]))
					i += j + 2 + k + 1
					continue
				}
			}
			b.WriteByte(s[i])
			i++
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}
