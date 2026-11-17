package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"minimalharness/internal/session"
	"minimalharness/internal/textutil"
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
				fmt.Fprintf(&b, "[assistant tool call] %s %s\n", tc.Function.Name, truncateForSummary(tc.Function.Arguments))
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
		return textutil.Prefix(s, 500) + "… [truncated in summary prompt]"
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
		if len(m.AnthropicContent) > 0 {
			// Text and tool input are already counted through shared fields.
			for _, block := range m.AnthropicContent {
				bytes += len(block.Thinking) + len(block.Signature) + len(block.Data) + 24
			}
		}
		images += len(m.Images)
		for _, tc := range m.ToolCalls {
			bytes += len(tc.Function.Arguments) + len(tc.Function.Name) + len(tc.ID) + 24
		}
	}
	return (bytes+2)/3 + images*2048
}

func (c *Config) promptView(msgs []llm.Message) []llm.Message {
	return truncateToolResults(session.RepairToolBatches(msgs), toolResultMaxChars)
}

func (c *Config) contextBudget() int {
	budget := int(float64(c.ContextLimit) * CompactThreshold)
	if c.Client != nil && c.Client.MaxTokens > 0 {
		budget = min(budget, c.ContextLimit-c.Client.MaxTokens)
	}
	return budget
}

// contextTokens combines measured usage with estimated changes since the last
// request. It also accounts for removed content when evaluating a checkpoint.
func (c *Config) contextTokens(view []llm.Message) int {
	estimated := estimatePromptTokens(view, c.Tools)
	projected := c.LastPromptTokens
	if c.lastPromptEstimate > 0 {
		projected += estimated - c.lastPromptEstimate
	}
	return max(estimated, projected)
}

func (c *Config) needsCompact(msgs []llm.Message) bool {
	return c.contextTokens(c.promptView(msgs)) >= c.contextBudget()
}

func (c *Config) compactInLoop(ctx context.Context, msgs []llm.Message) ([]llm.Message, llm.Usage, error) {
	if err := ctx.Err(); err != nil {
		return nil, llm.Usage{}, err
	}
	if pruned, ok := c.pruneCheckpoint(msgs); ok {
		return c.commitCheckpoint(pruned, llm.Usage{})
	}
	return c.compactMessages(ctx, msgs, true)
}

// pruneCheckpoint is attempted only at the context-budget boundary, never
// while rendering a normal request. Keep the latest tool batch intact and
// reclaim enough space for a substantial append-only interval (60% of the
// trigger budget). If old tool outputs cannot do that, generate a summary.
func (c *Config) pruneCheckpoint(msgs []llm.Message) ([]llm.Message, bool) {
	base := session.RepairToolBatches(msgs)
	if c.Session != nil {
		// A context hook may supply a transient rewrite. Persisting that
		// entire view would turn its temporary edits into session history.
		// Such turns use summary compaction instead of a raw-view checkpoint.
		var history []llm.Message
		for _, m := range base {
			if m.Role != "system" {
				history = append(history, m)
			}
		}
		saved, err := session.Load(c.Session.ID)
		if err != nil {
			return nil, false
		}
		// Compare persisted fields: hydrated image bytes deliberately stay
		// in memory and must not make an otherwise identical history differ.
		history, err = session.ImageReferences(history)
		if err != nil {
			return nil, false
		}
		currentJSON, currentErr := json.Marshal(history)
		savedJSON, savedErr := json.Marshal(saved)
		if currentErr != nil || savedErr != nil || !bytes.Equal(currentJSON, savedJSON) {
			return nil, false
		}
	}
	view := truncateToolResults(base, toolResultMaxChars)
	newestBatch := -1
	for i := len(view) - 1; i >= 0; i-- {
		if view[i].Role == "assistant" && len(view[i].ToolCalls) > 0 {
			newestBatch = i
			break
		}
	}
	if newestBatch < 0 {
		return nil, false
	}
	out := append([]llm.Message(nil), base...)
	target := c.contextBudget() * 3 / 5
	for i := 0; i < newestBatch; i++ {
		if view[i].Role != "tool" || len(view[i].Content) <= len(prunedToolResultPlaceholder) {
			continue
		}
		view[i].Content = prunedToolResultPlaceholder
		out[i].Content = prunedToolResultPlaceholder
		if c.contextTokens(view) <= target {
			if c.Session != nil && !session.CheckpointFits(out) {
				return nil, false
			}
			return out, true
		}
	}
	return nil, false
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
		if err := c.budgetRequest(); err != nil {
			return nil, llm.Usage{}, err
		}
		started := time.Now()
		var before int64
		if c.resources != nil {
			before = c.resources.attempts
		}
		summary, usage, err = c.CompactHistory(ctx, history)
		// This legacy callback cannot distinguish omitted usage from explicit zero.
		// Treat an all-zero report as unknown rather than invent evidence.
		var measured *llm.Usage
		if usage != (llm.Usage{}) {
			measured = &usage
		}
		if c.resources == nil || c.resources.attempts == before {
			c.recordBudgetUsage(measured, started, measured != nil, measured != nil)
		}
		if budgetErr := c.budgetError(); budgetErr != nil {
			err = budgetErr
		}
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
		// Context hooks may replace the task message entirely. The app's
		// current snapshot is appended after those hooks, so retain it
		// independently of whether a task marker remains in the view.
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Kind == "turn_context" {
				replacement = append(replacement, history[i])
				break
			}
		}
	}
	return c.commitCheckpoint(append(system, replacement...), usage)
}

// commitCheckpoint persists both pruning and summaries through the same atomic
// session marker. Resume therefore keeps exactly the view sent after a rewrite.
func (c *Config) commitCheckpoint(msgs []llm.Message, usage llm.Usage) ([]llm.Message, llm.Usage, error) {
	var system, replacement []llm.Message
	for _, m := range msgs {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			replacement = append(replacement, m)
		}
	}
	archive := ""
	var err error
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
