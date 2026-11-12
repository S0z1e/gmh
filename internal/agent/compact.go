package agent

import (
	"context"
	"fmt"
	"strings"

	"minimalharness/internal/llm"
)

// Compaction (P0-2d). When the context used by the last LLM call reaches
// 70% of the window, the conversation is summarised into a single message
// and the history is replaced by that summary. The original transcript is
// saved to a separate file first; the session file keeps a compact marker
// so --resume starts from the summary.

const (
	// DefaultContextLimit is the context window of deepseek-v4.1-flash
	// (1,048,576 tokens). Override with -context-limit for other models.
	DefaultContextLimit = 1048576
	// CompactThreshold is the fraction of the window that triggers compaction.
	CompactThreshold = 0.7
)

// ShouldCompact reports whether the last call's prompt tokens (the current
// context size, since history is resent in full each time) hit the trigger.
func ShouldCompact(lastPromptTokens, contextLimit int) bool {
	if contextLimit <= 0 {
		contextLimit = DefaultContextLimit
	}
	return lastPromptTokens >= int(float64(contextLimit)*CompactThreshold)
}

// BuildCompactPrompt renders the history plus the 6-section summary
// instructions used to produce the compaction summary.
// extras are additional instructions from session_before_compact
// hooks, already prefixed "[plugin <name>] "; they go after section 6
// and before the conversation, so a plugin can steer what the
// summary keeps without replacing it.
func BuildCompactPrompt(history []llm.Message, extras []string) string {
	var b strings.Builder
	b.WriteString("Summarise the conversation below into a compact brief the next assistant can continue from. Use exactly these sections, keep each tight, drop chatter:\n\n")
	b.WriteString("1. Task summary — what the user asked for and what was done\n")
	b.WriteString("2. Key files — paths touched and what changed in each\n")
	b.WriteString("3. Decisions — choices made and why\n")
	b.WriteString("4. Pending — what is unfinished or was deferred\n")
	b.WriteString("5. Errors and fixes — what failed and how it was resolved\n")
	b.WriteString("6. Next steps — what to do next\n\n")
	if len(extras) > 0 {
		b.WriteString("Additional instructions from plugins:\n")
		for _, e := range extras {
			b.WriteString(e + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("--- conversation ---\n")
	for _, m := range history {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "[user] %s\n", m.Content)
		case "assistant":
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant] %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[assistant tool call] %s %s\n", tc.Function.Name, tc.Function.Arguments)
			}
		case "tool":
			fmt.Fprintf(&b, "[tool result] %s\n", truncateForSummary(m.Content))
		}
	}
	return b.String()
}

// truncateForSummary keeps tool results short inside the summary prompt;
// the full results are already in the saved transcript.
func truncateForSummary(s string) string {
	if len(s) > 500 {
		return s[:500] + "… [truncated in summary prompt]"
	}
	return s
}

// Compact asks the model to summarise history. It is one extra LLM call
// with no tools; the caller replaces history with the returned summary.
// extras are passed through to BuildCompactPrompt.
func Compact(ctx context.Context, client *llm.Client, history []llm.Message, extras []string) (string, llm.Usage, error) {
	msgs := []llm.Message{
		{Role: "system", Content: "You write compact conversation summaries for a coding agent. Follow the requested sections exactly."},
		{Role: "user", Content: BuildCompactPrompt(history, extras)},
	}
	msg, usage, err := client.Chat(ctx, msgs, nil, nil)
	if err != nil {
		return "", usage, err
	}
	return msg.Content, usage, nil
}
