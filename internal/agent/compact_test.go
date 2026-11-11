package agent

import (
	"strings"
	"testing"

	"minimalharness/internal/llm"
)

func TestShouldCompact(t *testing.T) {
	cases := []struct {
		lastPrompt, limit int
		want              bool
	}{
		{7000, 10000, true},      // exactly 70%
		{6999, 10000, false},     // just below 70%
		{100, 0, false},          // 0 limit falls back to default 1,048,576
		{800000, 1048576, true},  // 76% of the default window
		{734002, 1048576, false}, // just below 70% of the default window
		{734003, 1048576, true},  // 70% of the default window (int truncation)
	}
	for _, c := range cases {
		if got := ShouldCompact(c.lastPrompt, c.limit); got != c.want {
			t.Errorf("ShouldCompact(%d, %d) = %v, want %v", c.lastPrompt, c.limit, got, c.want)
		}
	}
}

func TestBuildCompactPromptHasSixSections(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "add a test"},
		{Role: "assistant", Content: "done", ToolCalls: []llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "write", Arguments: "{}"}}}},
		{Role: "tool", Content: "created test.go", ToolCallID: "1"},
	}
	prompt := BuildCompactPrompt(history, nil)
	for _, section := range []string{
		"1. Task summary", "2. Key files", "3. Decisions",
		"4. Pending", "5. Errors and fixes", "6. Next steps",
	} {
		if !strings.Contains(prompt, section) {
			t.Fatalf("missing section %q", section)
		}
	}
	for _, want := range []string{"add a test", "done", "write", "created test.go"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("history content %q missing from prompt", want)
		}
	}
}

func TestBuildCompactPromptTruncatesToolResults(t *testing.T) {
	history := []llm.Message{
		{Role: "tool", Content: strings.Repeat("x", 2000), ToolCallID: "1"},
	}
	prompt := BuildCompactPrompt(history, nil)
	if strings.Contains(prompt, strings.Repeat("x", 2000)) {
		t.Fatal("tool result was not truncated in the summary prompt")
	}
	if !strings.Contains(prompt, "[truncated in summary prompt]") {
		t.Fatal("truncation marker missing")
	}
}

// Phase 4: session_before_compact extras go after section 6 and
// before the conversation, so a plugin steers what the summary
// keeps without replacing it.
func TestBuildCompactPromptExtrasPosition(t *testing.T) {
	history := []llm.Message{{Role: "user", Content: "do the thing"}}
	extras := []string{
		"[plugin plan] Keep the current plan in the summary.",
		"[plugin todo] Keep the open todos.",
	}
	prompt := BuildCompactPrompt(history, extras)
	sec6 := strings.Index(prompt, "6. Next steps")
	extra := strings.Index(prompt, "Additional instructions from plugins:")
	first := strings.Index(prompt, "[plugin plan]")
	second := strings.Index(prompt, "[plugin todo]")
	conv := strings.Index(prompt, "--- conversation ---")
	if sec6 < 0 || extra < 0 || first < 0 || second < 0 || conv < 0 {
		t.Fatalf("prompt missing a block:\n%s", prompt)
	}
	if !(sec6 < extra && extra < first && first < second && second < conv) {
		t.Fatalf("extras out of order (want section 6, extras block, plan, todo, conversation):\n%s", prompt)
	}
}

func TestBuildCompactPromptNoExtrasBlock(t *testing.T) {
	history := []llm.Message{{Role: "user", Content: "do the thing"}}
	if prompt := BuildCompactPrompt(history, nil); strings.Contains(prompt, "Additional instructions") {
		t.Fatalf("extras block present with no extras:\n%s", prompt)
	}
	if prompt := BuildCompactPrompt(history, []string{}); strings.Contains(prompt, "Additional instructions") {
		t.Fatalf("extras block present with empty extras:\n%s", prompt)
	}
}
