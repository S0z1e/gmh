package tui

import (
	"strings"
	"testing"
)

// The markdown subset: bold, inline code, headings, links, quotes,
// list markers and fenced code blocks. Plain text passes through
// byte-identical (apart from the 3-space indent renderText adds).
func TestFormatMarkdown(t *testing.T) {
	cases := []struct {
		in   string
		want string // visible text after stripping ANSI
	}{
		{"plain text", "plain text"},
		{"a **bold** word", "a bold word"},
		{"run `ls -la` now", "run ls -la now"},
		{"# Title", "Title"},
		{"## Section", "Section"},
		{"> quoted", "quoted"},
		{"- item one", "- item one"},
		{"see [docs](https://example.com) here", "see docs here"},
		{"unclosed **bold", "unclosed **bold"},
		{"unclosed `code", "unclosed `code"},
		{"[not a link]", "[not a link]"},
	}
	for _, tc := range cases {
		got := stripANSI(formatMarkdown(tc.in))
		if got != tc.want {
			t.Errorf("formatMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A fenced block: the fence markers disappear, the content stays
	// (with the extra 2-space fence indent formatMarkdown adds).
	fenced := "before\n```go\nx := 1\n```\nafter"
	got := stripANSI(formatMarkdown(fenced))
	if got != "before\n  x := 1\nafter" {
		t.Fatalf("fenced block = %q", got)
	}
	// Styling is actually applied (not just the literal text).
	styled := formatMarkdown("a **bold** word")
	if styled == "a **bold** word" {
		t.Fatal("bold must be styled, not literal")
	}
	if !strings.Contains(styled, "bold") {
		t.Fatalf("bold text lost: %q", styled)
	}
}

func TestRenderTextIndents(t *testing.T) {
	got := stripANSI(renderText("one\ntwo"))
	if got != "   one\n   two" {
		t.Fatalf("renderText = %q, want 3-space indent per line", got)
	}
}
