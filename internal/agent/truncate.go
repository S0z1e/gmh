package agent

import (
	"fmt"

	"minimalharness/internal/llm"
)

// Tool-result truncation (P0-2b). The session JSONL keeps the full
// observations; only the view sent to the LLM is truncated, so a huge
// early output cannot blow up the context of later steps.
//
//   - The newest keepFull tool results are kept up to recentMaxChars.
//   - Older tool results are cut to oldMaxChars.
//   - Truncation keeps head + tail, never just the head, and always
//     says how much was dropped.
func truncateToolResults(msgs []llm.Message, keepFull, oldMaxChars, recentMaxChars int) []llm.Message {
	// Count tool messages from the end to find which are "recent".
	toolIdx := make([]int, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	recentStart := len(toolIdx) - keepFull
	if recentStart < 0 {
		recentStart = 0
	}
	recent := map[int]bool{}
	for _, i := range toolIdx[recentStart:] {
		recent[i] = true
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		max := oldMaxChars
		if recent[i] {
			max = recentMaxChars
		}
		out[i].Content = capText(m.Content, max)
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
