package agent

import (
	"fmt"

	"minimalharness/internal/llm"
)

// Tool-result truncation (P0-2b). The session JSONL keeps the full
// observations; only the view sent to the LLM is truncated, so a huge
// early output cannot blow up the context of later steps.
//
// The cap is UNIFORM: every tool result gets the same maxChars,
// however old it is. An earlier version kept the newest 2 results
// full (32 KiB) and cut older ones to 2,000 chars, so a result's sent
// form changed every step as newer results arrived — the prefix the
// provider had cached no longer matched, and cache hits collapsed
// (measured 9.6% over a 100-step run, 11/100 steps hitting, 0
// otherwise). With one cap, the sent view is a deterministic function
// of the append-only history: a message, once sent, is resent
// byte-identical forever, which is what prefix caching keys on — the
// same shape opencode (truncate once at tool-return time) and Codex
// (truncate in record_items) use. The price is that old results stay
// up to maxChars instead of shrinking to 2,000, so prompts grow
// faster on long runs; the compaction trigger bounds that growth.
const toolResultMaxChars = 32 * 1024

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
