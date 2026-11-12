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

func TestTruncateToolResultsUniformCap(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		toolMsg(big), // old
		toolMsg(big), // old
		toolMsg(big), // recent
	}
	out := truncateToolResults(msgs, 32*1024)
	if out[0].Content != "sys" || out[1].Content != "do it" {
		t.Fatal("non-tool messages must be untouched")
	}
	// One cap for old and new alike: nothing over the cap is sent
	// whole, and nothing under it is touched.
	for _, i := range []int{2, 3, 4} {
		if len(out[i].Content) != 5000 {
			t.Fatalf("tool result %d changed under the cap: %d chars", i, len(out[i].Content))
		}
	}
	// Input must not be mutated.
	if len(msgs[2].Content) != 5000 {
		t.Fatal("truncateToolResults mutated its input")
	}
}

func TestTruncateToolResultsCapsLarge(t *testing.T) {
	big := strings.Repeat("x", 40000)
	msgs := []llm.Message{toolMsg(big)}
	out := truncateToolResults(msgs, 32*1024)
	if len(out[0].Content) > 33*1024 {
		t.Fatalf("tool result not capped: %d chars", len(out[0].Content))
	}
	if !strings.Contains(out[0].Content, "[truncated:") {
		t.Fatal("truncation marker missing")
	}
}

// TestTruncateSentFormIsStable is the prefix-cache property: once a
// tool result has been sent, appending newer results must not change
// its sent form. The old sliding window (newest 2 full, older cut to
// 2,000 chars) broke this on every step.
func TestTruncateSentFormIsStable(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		toolMsg(big),
	}
	first := truncateToolResults(msgs, 32*1024)
	// Three more steps append more tool results (one over the cap).
	msgs = append(msgs,
		toolMsg(strings.Repeat("y", 40000)),
		toolMsg("small"),
		toolMsg(strings.Repeat("z", 3000)),
	)
	later := truncateToolResults(msgs, 32*1024)
	if later[1].Content != first[1].Content {
		t.Fatalf("already-sent tool result changed:\nbefore %d chars\n after %d chars", len(first[1].Content), len(later[1].Content))
	}
	if len(later[2].Content) > 33*1024 || !strings.Contains(later[2].Content, "[truncated:") {
		t.Fatalf("over-cap result not truncated the same way: %d chars", len(later[2].Content))
	}
}
