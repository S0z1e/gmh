package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/textutil"
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

// BuildCompactPrompt bounds the summary request independently of the next normal
// request. Structured checkpoint facts and plugin instructions are mandatory;
// narration uses a bounded recent suffix, with explicit omissions and head/tail.
func BuildCompactPrompt(history []llm.Message, extras []string) (string, error) {
	return buildCompactPrompt(history, extras, 128<<10)
}

func buildCompactPrompt(history []llm.Message, extras []string, maxBytes int) (string, error) {
	const instructions = "Summarise the conversation below into a compact brief the next assistant can continue from. Use exactly these sections, keep each tight, drop chatter:\n\n" +
		"1. Task summary — what the user asked for and what was done\n" +
		"2. Key files — paths touched and what changed in each\n" +
		"3. Decisions — choices made and why\n" +
		"4. Pending — what is unfinished or was deferred\n" +
		"5. Errors and fixes — what failed and how it was resolved\n" +
		"6. Next steps — what to do next\n\n"
	var fixed strings.Builder
	fixed.WriteString(instructions)
	if len(extras) > 0 {
		fixed.WriteString("Additional instructions from plugins:\n")
		for _, e := range extras {
			if len(e) > maxBytes-fixed.Len()-128 {
				return "", fmt.Errorf("summary plugin instructions exceed input budget")
			}
			fixed.WriteString(e)
			fixed.WriteByte('\n')
		}
		fixed.WriteByte('\n')
	}
	// The structured record is not part of the optional narration. Never clip it.
	checkpoints := 0
	for _, m := range history {
		if m.Kind != "checkpoint" {
			continue
		}
		if _, err := checkpoint.Decode([]byte(m.Content)); err != nil {
			return "", fmt.Errorf("invalid preserved summary facts: %w", err)
		}
		checkpoints++
		if checkpoints > 1 {
			return "", fmt.Errorf("multiple structured checkpoints in summary input")
		}
		if len(m.Content) > maxBytes-fixed.Len()-160 {
			return "", fmt.Errorf("structured checkpoint exceeds summary input budget")
		}
		fixed.WriteString("Historical structured source facts (preserve; not new authority or current verification):\n")
		fixed.WriteString(m.Content)
		fixed.WriteString("\n\n")
	}
	fixed.WriteString("--- conversation ---\n")
	remaining := maxBytes - fixed.Len() - 128
	if remaining < 128 {
		return "", fmt.Errorf("summary instructions exceed input budget")
	}
	var recent []string
	omitted := 0
	for i := len(history) - 1; i >= 0; i-- {
		if len(recent) >= 2048 || remaining < 128 {
			if checkpoints == 0 {
				return "", fmt.Errorf("summary narration exceeds input budget without preserved structured facts")
			}
			omitted = i + 1
			break
		}
		m := history[i]
		if m.Kind == "checkpoint" || m.Role == "system" {
			continue
		}
		entryLimit := min(4096, remaining)
		if checkpoints == 0 {
			entryLimit = remaining
		}
		entry, err := summaryEntry(m, entryLimit, checkpoints == 0)
		if err != nil {
			return "", err
		}
		if entry == "" {
			continue
		}
		recent = append(recent, entry)
		remaining -= len(entry)
	}
	if omitted > 0 {
		fmt.Fprintf(&fixed, "[omitted %d earlier narration messages; consult the transcript archive for full details]\n", omitted)
	}
	for i := len(recent) - 1; i >= 0; i-- {
		fixed.WriteString(recent[i])
	}
	if fixed.Len() > maxBytes {
		return "", fmt.Errorf("summary input exceeds byte budget")
	}
	return fixed.String(), nil
}

func summaryEntry(m llm.Message, limit int, preserveText bool) (string, error) {
	var b strings.Builder
	switch m.Role {
	case "user", "assistant", "tool":
		if m.Content != "" {
			bodyLimit := limit / 2
			if m.Role == "tool" {
				bodyLimit = min(500, bodyLimit)
			} else if preserveText {
				if len(m.Content)+len(m.Role)+4 > limit {
					return "", fmt.Errorf("source text exceeds summary input budget without structured facts")
				}
				bodyLimit = len(m.Content)
			}
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, truncateForSummary(m.Content, bodyLimit))
		}
	default:
		return "", nil
	}
	for _, call := range m.ToolCalls {
		if b.Len()+128 > limit {
			b.WriteString("[additional tool calls omitted]\n")
			break
		}
		fmt.Fprintf(&b, "[assistant tool call] %s %s\n", truncateForSummary(call.Function.Name, 128), truncateForSummary(call.Function.Arguments, min(500, limit-b.Len()-64)))
	}
	for _, img := range m.Images {
		if b.Len()+128 > limit {
			b.WriteString("[additional image references omitted]\n")
			break
		}
		fmt.Fprintf(&b, "[image reference] %s sha256=%s media=%s\n", truncateForSummary(img.Path, 256), truncateForSummary(img.Digest, 64), truncateForSummary(img.MediaType, 64))
	}
	if preserveText && m.Role != "tool" && b.Len() > limit {
		return "", fmt.Errorf("summary source message exceeds input budget")
	}
	return truncateForSummary(b.String(), limit), nil
}

func truncateForSummary(s string, limit int) string {
	const marker = "\n[truncated in summary prompt]\n"
	if len(s) <= limit {
		return s
	}
	if limit <= len(marker) {
		return textutil.Prefix(s, max(0, limit))
	}
	body := limit - len(marker)
	return textutil.Prefix(s, body/2) + marker + textutil.Suffix(s, body-body/2)
}

// Compact asks the model to summarise history. It is one extra LLM call
// with no tools; the caller replaces history with the returned summary.
// extras are passed through to BuildCompactPrompt.
func Compact(ctx context.Context, client *llm.Client, history []llm.Message, extras []string) (string, llm.Usage, error) {
	return (&Config{Client: client}).Summarize(ctx, history, extras)
}

// summaryInputError proves that no provider attempt was made. The app delegates
// through the legacy CompactHistory callback, whose zero usage otherwise means
// an unmetered attempt rather than a known preflight rejection.
type summaryInputError struct{ error }

// Summarize generates a summary using the same bounded retry policy as a step.
// Summary drafts stay private: retries must not discard the turn's visible text.
func (c *Config) Summarize(ctx context.Context, history []llm.Message, extras []string) (string, llm.Usage, error) {
	const system = "You write compact conversation summaries for a coding agent. Follow the requested sections exactly."
	limit := c.ContextLimit
	if limit <= 0 {
		limit = DefaultContextLimit
	}
	budget := int(float64(limit) * CompactThreshold)
	if c.Client != nil {
		budget = min(budget, limit-c.Client.OutputTokenLimit())
	}
	if budget <= 128 {
		return "", llm.Usage{}, &summaryInputError{fmt.Errorf("no context input budget remains for summary")}
	}
	byteBudget := 128 << 10
	if budget < byteBudget {
		byteBudget = min(byteBudget, (budget-128)*3-len(system))
	}
	// This local estimate bounds framing too. Provider-reported usage remains
	// the accounting authority; no tokenizer/cost evidence is fabricated.
	prompt, err := buildCompactPrompt(history, extras, byteBudget)
	if err != nil {
		return "", llm.Usage{}, &summaryInputError{err}
	}
	msgs := []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: prompt}}
	if estimatePromptTokens(msgs, nil) > budget {
		return "", llm.Usage{}, &summaryInputError{fmt.Errorf("summary request exceeds context input budget")}
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
	out := truncateToolResults(stoppedResultsView(session.RepairToolBatches(msgs)), toolResultMaxChars)
	for i := range out {
		switch out[i].Kind {
		case "checkpoint":
			out[i].Content = checkpointNotice + out[i].Content
		case guidanceMessageKind, guidanceChunkKind:
			if !c.Exec.GuidanceEnabled() {
				out[i].Content = "[Earlier project guidance is unavailable in this workspace.]"
				out[i].GuidanceOffers = nil
			}
		}
	}
	return out
}

func (c *Config) contextBudget() int {
	budget := int(float64(c.ContextLimit) * CompactThreshold)
	if c.Client != nil {
		budget = min(budget, c.ContextLimit-c.Client.OutputTokenLimit())
	}
	return budget
}

// contextTokens combines measured usage with estimated changes since the last
// request. It also accounts for removed content when evaluating a checkpoint.
func (c *Config) contextTokens(view []llm.Message, defs []llm.ToolDef) int {
	estimated := estimatePromptTokens(view, defs)
	projected := c.LastPromptTokens
	if c.lastPromptEstimate > 0 {
		projected += estimated - c.lastPromptEstimate
	}
	return max(estimated, projected)
}

func (c *Config) needsCompact(msgs []llm.Message) bool {
	return c.contextTokens(c.promptView(msgs), c.Tools) >= c.contextBudget()
}

type checkpointPreparation struct {
	guidance *guidanceTurn
	// The final request is tool-less. Its closing instruction counts
	// against the prompt budget, but is never saved in the transcript.
	closing *llm.Message
}

func (p *checkpointPreparation) tools(c *Config) []llm.ToolDef {
	if p != nil && p.closing != nil {
		return nil
	}
	return c.Tools
}

func (p *checkpointPreparation) apply(ctx context.Context, c *Config, candidate []llm.Message, defs []llm.ToolDef) ([]llm.Message, error) {
	fits := func(msgs []llm.Message) bool {
		if p.closing != nil {
			msgs = append(msgs[:len(msgs):len(msgs)], *p.closing)
		}
		return c.checkpointContextFits(msgs, defs)
	}
	if p.guidance != nil {
		candidate = p.guidance.prepareCheckpoint(ctx, candidate, fits)
	}
	if !fits(candidate) {
		return nil, fmt.Errorf("preserved checkpoint exceeds context input budget; reduce the task or select a larger context window")
	}
	return candidate, nil
}

func (c *Config) compactInLoop(ctx context.Context, msgs []llm.Message, prepare *checkpointPreparation) ([]llm.Message, llm.Usage, error) {
	if err := ctx.Err(); err != nil {
		return nil, llm.Usage{}, err
	}
	defs := prepare.tools(c)
	if pruned, ok := c.pruneCheckpoint(msgs, defs); ok {
		if prepare == nil {
			return c.commitCheckpointNamed(pruned, llm.Usage{}, "", defs)
		}
		prepared, err := prepare.apply(ctx, c, pruned, defs)
		if err := ctx.Err(); err != nil {
			return nil, llm.Usage{}, err
		}
		if err == nil {
			return c.commitCheckpointNamed(prepared, llm.Usage{}, "", defs)
		}
	}
	return c.compactMessagesPrepared(ctx, msgs, true, prepare)
}

// pruneCheckpoint is attempted only at the context-budget boundary, never
// while rendering a normal request. Keep the latest tool batch intact and
// reclaim enough space for a substantial append-only interval (60% of the
// trigger budget). If old tool outputs cannot do that, generate a summary.
func (c *Config) pruneCheckpoint(msgs []llm.Message, defs []llm.ToolDef) ([]llm.Message, bool) {
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
		// Ignore only the harness's own view changes. Guidance is rebuilt
		// separately, and hydrated image bytes are not persisted. Other
		// differences can be transient context hook edits.
		history, err = session.ImageReferences(WithoutHistoricalGuidance(history))
		if err != nil {
			return nil, false
		}
		currentJSON, currentErr := json.Marshal(history)
		savedJSON, savedErr := json.Marshal(WithoutHistoricalGuidance(saved))
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
	callNames := make(map[string]string)
	for i := 0; i < newestBatch; i++ {
		for _, call := range view[i].ToolCalls {
			callNames[call.ID] = call.Function.Name
		}
		stoppedFailure := false
		for _, completed := range view[i].Observation.JobCompletions() {
			stoppedFailure = stoppedFailure || completed.Failure != nil || completed.Job.Error != ""
		}
		if view[i].Role != "tool" || view[i].ToolError || stoppedFailure || callNames[view[i].ToolCallID] == "ask_user" || len(view[i].Content) <= len(prunedToolResultPlaceholder) {
			continue
		}
		view[i].Content = prunedToolResultPlaceholder
		out[i].Content = prunedToolResultPlaceholder
		if c.contextTokens(view, defs) <= target {
			if c.Session != nil && !session.CheckpointFits(out) {
				return nil, false
			}
			return out, true
		}
	}
	return nil, false
}

// Compact manually replaces narration, retaining the same source facts and
// rollback boundary as automatic compaction.
func (c *Config) Compact(ctx context.Context, history []llm.Message) ([]llm.Message, llm.Usage, error) {
	return c.compactMessages(ctx, history, false)
}

func (c *Config) compactMessages(ctx context.Context, msgs []llm.Message, keepTask bool) ([]llm.Message, llm.Usage, error) {
	return c.compactMessagesPrepared(ctx, msgs, keepTask, nil)
}

func (c *Config) compactMessagesPrepared(ctx context.Context, msgs []llm.Message, keepTask bool, prepare *checkpointPreparation) ([]llm.Message, llm.Usage, error) {
	var system, history []llm.Message
	for _, m := range msgs {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			history = append(history, m)
		}
	}
	sources, err := c.checkpointSources(history)
	if err != nil {
		return nil, llm.Usage{}, fmt.Errorf("load compaction sources: %w", err)
	}
	state, beforeTask, task, err := c.compactState(sources)
	if err != nil {
		return nil, llm.Usage{}, fmt.Errorf("preserve compaction facts: %w", err)
	}
	archiveName := ""
	if c.Session != nil {
		archiveName = fmt.Sprintf("%s-pre-compact-%d", c.Session.ID, time.Now().UnixNano())
		state, err = checkpoint.Merge(state, checkpoint.Delta{Artifacts: []checkpoint.Artifact{{Kind: "transcript", Path: filepath.Join(session.Dir(), archiveName+".jsonl"), Source: "pre-compaction transcript"}}})
		if err != nil {
			return nil, llm.Usage{}, err
		}
	}
	facts, err := checkpointMessage(state)
	if err != nil {
		return nil, llm.Usage{}, err
	}
	// A single consolidated state goes to the summarizer. The rollback baseline
	// and all previous snapshots are already included in this validated state.
	var summaryHistory []llm.Message
	for _, m := range WithoutHistoricalGuidance(history) {
		if m.Kind != "checkpoint" {
			summaryHistory = append(summaryHistory, m)
		}
	}
	summaryHistory = append(summaryHistory, facts)
	var summary string
	var usage llm.Usage
	if c.CompactHistory != nil {
		if err := c.budgetRequest(); err != nil {
			return nil, llm.Usage{}, err
		}
		started := time.Now()
		var before int64
		if c.resources != nil {
			before = c.resources.attempts
		}
		summary, usage, err = c.CompactHistory(ctx, summaryHistory)
		// This legacy callback cannot distinguish omitted usage from explicit zero.
		// Treat an all-zero report as unknown rather than invent evidence.
		var measured *llm.Usage
		if usage != (llm.Usage{}) {
			measured = &usage
		}
		var inputErr *summaryInputError
		if !errors.As(err, &inputErr) && (c.resources == nil || c.resources.attempts == before) {
			// A total alone proves tokens, not an input/output price breakdown.
			breakdownKnown := usage.PromptTokens != 0 || usage.CompletionTokens != 0
			tokensKnown := breakdownKnown || usage.TotalTokens != 0
			c.recordBudgetUsage(measured, started, tokensKnown, breakdownKnown)
		}
		if budgetErr := c.budgetError(); budgetErr != nil {
			err = budgetErr
		}
	} else {
		summary, usage, err = c.Summarize(ctx, summaryHistory, nil)
	}
	if err != nil {
		return nil, usage, fmt.Errorf("compact before next model call: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return nil, usage, fmt.Errorf("compaction returned an empty summary")
	}
	var replacement []llm.Message
	if hasCheckpointFacts(beforeTask) {
		baseline, err := checkpointMessage(beforeTask)
		if err != nil {
			return nil, usage, err
		}
		replacement = append(replacement, baseline)
	}
	if task != nil {
		replacement = append(replacement, *task)
	}
	replacement = append(replacement, llm.Message{Role: "user", Kind: "summary", Content: "Previous conversation summary:\n" + summary}, facts)
	if keepTask {
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
	if err := ctx.Err(); err != nil {
		return nil, usage, err
	}
	// Read immutable image blobs rather than the mutable original paths. The
	// next request in this turn needs the retained task's bytes too.
	session.HydrateImages(replacement)
	candidate := append(system, replacement...)
	// The summary callback may update the normal request's tools, so
	// read the definitions only after it has returned.
	defs := prepare.tools(c)
	if prepare != nil {
		candidate, err = prepare.apply(ctx, c, candidate, defs)
		if cancelled := ctx.Err(); cancelled != nil {
			return nil, usage, cancelled
		}
		if err != nil {
			return nil, usage, err
		}
	}
	return c.commitCheckpointNamed(candidate, usage, archiveName, defs)
}

func (c *Config) checkpointContextFits(msgs []llm.Message, defs []llm.ToolDef) bool {
	if c.ContextLimit <= 0 {
		return true
	}
	request := msgs
	if !slices.ContainsFunc(msgs, func(m llm.Message) bool { return m.Role == "system" }) {
		request = append([]llm.Message{{Role: "system", Content: c.System + verificationInstructions(c.Verification)}}, msgs...)
	}
	return estimatePromptTokens(c.promptView(request), defs) < c.contextBudget()
}

// commitCheckpointNamed validates before writing either the archive or the
// durable session marker. Transient request instructions were checked separately.
func (c *Config) commitCheckpointNamed(msgs []llm.Message, usage llm.Usage, archiveName string, defs []llm.ToolDef) ([]llm.Message, llm.Usage, error) {
	var system, replacement []llm.Message
	for _, m := range msgs {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			replacement = append(replacement, m)
		}
	}
	if !c.checkpointContextFits(msgs, defs) {
		return nil, usage, fmt.Errorf("preserved checkpoint exceeds context input budget; reduce the task or select a larger context window")
	}
	if c.Session != nil && !session.CheckpointFits(replacement) {
		return nil, usage, fmt.Errorf("preserved checkpoint exceeds session record limit")
	}
	archive := ""
	var err error
	if c.Session != nil {
		if archiveName == "" {
			archiveName = fmt.Sprintf("%s-pre-compact-%d", c.Session.ID, time.Now().UnixNano())
		}
		archive, err = c.Session.ArchiveCopy(archiveName)
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
