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
// faster on long runs; threshold-gated pruning (below) and the
// compaction trigger bound that growth.
const toolResultMaxChars = 32 * 1024

// Threshold-gated prune (cache item 4). When the truncated history
// exceeds pruneThresholdChars, the tool results in all but the newest
// pruneKeepNewestBlocks blocks are replaced by one fixed placeholder.
// This caps the prompt of long runs instead of letting it grow until
// compaction, which saves prompt tokens (the "cost" the user asked
// for) — at the price of one mostly-cold step each time a block is
// newly pruned, while the model loses old file contents it can
// re-read by name (the assistant's tool calls stay visible, so it
// knows what was read).
//
// The prune is SAFE for prefix caching only because its boundaries
// are frozen: tool messages are grouped into blocks by cumulative
// content chars, walking from the start of the append-only history,
// so a block's boundaries never move once formed; a block, once
// pruned, stays pruned (the total only grows, and the newest-blocks
// cutoff only moves forward); and the placeholder is one fixed
// string with no counts, so it never changes. Between prune events
// the sent view is append-only, exactly like truncation. A
// "prune until under threshold" rule that moved the boundary every
// step would reintroduce the sliding-window bug that collapsed
// cache hits — that is deliberately NOT what this does.
//
// Only tool messages are ever pruned, and only their Content is
// replaced: Role and ToolCallID stay, so the tool-call linkage is
// intact and the API still accepts the history. User and assistant
// messages are never pruned. The session JSONL keeps the full
// observations; pruning touches only the view sent to the LLM.
const (
	pruneThresholdChars   = 48 * 1024
	pruneBlockChars       = 24 * 1024
	pruneKeepNewestBlocks = 1
)

const prunedToolResultPlaceholder = "[earlier tool output pruned to save context]"

// pruneOldToolResults replaces the content of tool results in old
// blocks with prunedToolResultPlaceholder when the total truncated
// tool content exceeds thresholdChars. It is pure and deterministic:
// the same history always yields the same sent view, and a block's
// pruned state never reverses. It never mutates its input. When the
// total is under the threshold it returns msgs unchanged.
func pruneOldToolResults(msgs []llm.Message, thresholdChars, blockChars, keepNewestBlocks int) []llm.Message {
	total := 0
	for _, m := range msgs {
		if m.Role == "tool" {
			total += len(m.Content)
		}
	}
	if total <= thresholdChars {
		return msgs
	}
	// Group tool messages into blocks by cumulative content chars.
	// blockOf[i] is the block index of msgs[i], or -1 for non-tool
	// messages. A block holds whole messages; a single message over
	// blockChars forms a block by itself.
	blockOf := make([]int, len(msgs))
	block := 0
	blockCharsUsed := 0
	for i, m := range msgs {
		if m.Role != "tool" {
			blockOf[i] = -1
			continue
		}
		if blockCharsUsed > 0 && blockCharsUsed+len(m.Content) > blockChars {
			block++
			blockCharsUsed = 0
		}
		blockOf[i] = block
		blockCharsUsed += len(m.Content)
	}
	nBlocks := block + 1
	keepFrom := nBlocks - keepNewestBlocks
	if keepFrom <= 0 {
		return msgs
	}
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i, m := range msgs {
		if m.Role == "tool" && blockOf[i] >= 0 && blockOf[i] < keepFrom {
			out[i].Content = prunedToolResultPlaceholder
		}
	}
	return out
}

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
