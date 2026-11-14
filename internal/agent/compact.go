package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"minimalharness/internal/session"
	"strings"
	"time"

	"minimalharness/internal/llm"
)

// Compaction (P0-2d). When the context used by the last LLM call reaches
// 70% of the window, the conversation is summarised into a single message
// and the history is replaced by that summary. The original transcript is
// saved to a separate file first; the session file keeps a compact marker
// so --resume starts from the summary.

const (
	// DefaultContextLimit matches the conservative config fallback.
	DefaultContextLimit = 128000
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
	return (&Config{Client: client}).Summarize(ctx, history, extras)
}

// Summarize generates a summary using the same bounded retry policy as a step.
// Summary drafts stay private: retries must not discard the turn's visible text.
func (c *Config) Summarize(ctx context.Context, history []llm.Message, extras []string) (string, llm.Usage, error) {
	msgs := []llm.Message{
		{Role: "system", Content: "You write compact conversation summaries for a coding agent. Follow the requested sections exactly."},
		{Role: "user", Content: BuildCompactPrompt(history, extras)},
	}
	var msg llm.Message
	var usage llm.Usage
	attempts, err := c.streamStep(ctx, "Compaction", msgs, nil, nil, func(m llm.Message, u llm.Usage) {
		msg, usage = m, u
	}, nil, nil)
	if err != nil {
		if attempts > 1 {
			err = fmt.Errorf("%w (after %d attempts)", err, attempts)
		}
		return "", usage, err
	}
	if msg.FinishReason == "length" || msg.FinishReason == "max_tokens" || msg.FinishReason == "incomplete" {
		return "", usage, fmt.Errorf("summary was truncated by the model output limit")
	}
	return msg.Content, usage, nil
}

// estimatePromptTokens is a conservative local guard, not provider usage.
// Schema and framing overhead count too; image cost varies by provider.
func estimatePromptTokens(msgs []llm.Message, defs []llm.ToolDef) int {
	schemas, _ := json.Marshal(defs)
	bytes, images := len(schemas), 0
	for _, m := range msgs {
		bytes += len(m.Content) + len(m.Role) + len(m.ToolCallID) + 24
		images += len(m.Images)
		for _, tc := range m.ToolCalls {
			bytes += len(tc.Function.Arguments) + len(tc.Function.Name) + len(tc.ID) + 24
		}
	}
	return (bytes+2)/3 + images*2048
}

func (c *Config) promptView(msgs []llm.Message) []llm.Message {
	view := truncateToolResults(session.RepairToolBatches(msgs), toolResultMaxChars)
	return pruneOldToolResults(view, pruneThresholdChars, pruneBlockChars, pruneKeepNewestBlocks)
}

// needsCompact combines measured usage with growth since the last request.
func (c *Config) needsCompact(msgs []llm.Message) bool {
	budget := int(float64(c.ContextLimit) * CompactThreshold)
	if c.Client != nil && c.Client.MaxTokens > 0 {
		budget = min(budget, c.ContextLimit-c.Client.MaxTokens)
	}
	estimated := estimatePromptTokens(c.promptView(msgs), c.Tools)
	projected := c.LastPromptTokens
	if c.lastPromptEstimate > 0 {
		projected += max(0, estimated-c.lastPromptEstimate)
	}
	return max(estimated, projected) >= budget
}

func (c *Config) compactInLoop(ctx context.Context, msgs []llm.Message) ([]llm.Message, llm.Usage, error) {
	return c.compactMessages(ctx, msgs, true)
}

// Compact manually replaces history by a summary, using the same checkpoint as
// automatic compaction. The automatic path additionally retains the active task.
func (c *Config) Compact(ctx context.Context, history []llm.Message) ([]llm.Message, llm.Usage, error) {
	return c.compactMessages(ctx, history, false)
}

func (c *Config) compactMessages(ctx context.Context, msgs []llm.Message, keepTask bool) ([]llm.Message, llm.Usage, error) {
	var system, history []llm.Message
	for _, m := range msgs {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			history = append(history, m)
		}
	}
	var summary string
	var usage llm.Usage
	var err error
	if c.CompactHistory != nil {
		summary, usage, err = c.CompactHistory(ctx, history)
	} else {
		summary, usage, err = c.Summarize(ctx, history, nil)
	}
	if err != nil {
		return nil, usage, fmt.Errorf("compact before next model call: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return nil, usage, fmt.Errorf("compaction returned an empty summary")
	}
	replacement := []llm.Message{{Role: "user", Kind: "summary", Content: "Previous conversation summary:\n" + summary}}
	if keepTask {
		for i := len(history) - 1; i >= 0; i-- {
			if session.IsTurnStart(history[i]) {
				replacement = append(replacement, history[i])
				break
			}
		}
	}
	archive := ""
	if c.Session != nil {
		archive, err = c.Session.ArchiveCopy(fmt.Sprintf("%s-pre-compact-%d", c.Session.ID, time.Now().UnixNano()))
		if err == nil {
			err = c.Session.MarkCompactMessages(replacement)
		}
		if err != nil {
			return nil, usage, err
		}
	}
	c.LastPromptTokens = 0
	c.lastPromptEstimate = 0
	if c.OnCompact != nil {
		c.OnCompact(replacement, usage, archive)
	}
	return append(system, replacement...), usage, nil
}
