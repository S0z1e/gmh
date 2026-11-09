package agent

import (
	"strings"
	"testing"

	"minimalharness/internal/llm"
)

func toolMsg(content string) llm.Message {
	return llm.Message{Role: "tool", Content: content, ToolCallID: "1"}
}

func TestCapTextKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
	out := capText(s, 40)
	if len(out) > 120 {
		t.Fatalf("capText too long: %d chars", len(out))
	}
	if !strings.HasPrefix(out, strings.Repeat("a", 30)) {
		t.Fatalf("head not kept:\n%s", out)
	}
	if !strings.HasSuffix(out, strings.Repeat("b", 10)) {
		t.Fatalf("tail not kept:\n%s", out)
	}
	if !strings.Contains(out, "[truncated: 40 chars shown of 206]") {
		t.Fatalf("marker missing:\n%s", out)
	}
}

func TestCapTextLeavesShortUntouched(t *testing.T) {
	if got := capText("short", 100); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
}

func TestTruncateOldToolResults(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		toolMsg(big), // old
		toolMsg(big), // old
		toolMsg(big), // recent
	}
	out := truncateToolResults(msgs, 1, 2000, 32*1024)
	if out[0].Content != "sys" || out[1].Content != "do it" {
		t.Fatal("non-tool messages must be untouched")
	}
	for _, i := range []int{2, 3} {
		if len(out[i].Content) > 2200 || !strings.Contains(out[i].Content, "[truncated:") {
			t.Fatalf("old tool result %d not truncated: %d chars", i, len(out[i].Content))
		}
	}
	if len(out[4].Content) != 5000 {
		t.Fatalf("recent tool result should stay full (5000), got %d", len(out[4].Content))
	}
	// Input must not be mutated.
	if len(msgs[2].Content) != 5000 {
		t.Fatal("truncateToolResults mutated its input")
	}
}

func TestTruncateRecentAlsoCapped(t *testing.T) {
	big := strings.Repeat("x", 40000)
	msgs := []llm.Message{toolMsg(big)}
	out := truncateToolResults(msgs, 1, 2000, 32*1024)
	if len(out[0].Content) > 33*1024 {
		t.Fatalf("recent tool result not capped: %d chars", len(out[0].Content))
	}
}
