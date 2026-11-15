package agent

import (
	"fmt"

	"minimalharness/internal/llm"
)

// Each tool result has one deterministic cap. Between context checkpoints,
// appending history never changes a result that has already been sent. The
// session retains full observations until a checkpoint archives and replaces
// older content; ordinary prompt rendering never ages or prunes messages.
const toolResultMaxChars = 32 * 1024

const prunedToolResultPlaceholder = "[earlier tool output pruned to save context]"

func truncateToolResults(msgs []llm.Message, maxChars int) []llm.Message {
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		out[i].Content = capText(m.Content, maxChars)
	}
	return out
}

// capText bounds s to max chars, keeping head + tail with a marker.
func capText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max * 3 / 4
	tail := max - head
	return fmt.Sprintf("%s\n... [truncated: %d chars shown of %d] ...\n%s",
		s[:head], max, len(s), s[len(s)-tail:])
}
