package agent

import (
	"context"
	"fmt"
	"strings"

	"minimalharness/internal/llm"
)

type streamedStep struct {
	assistant llm.Message
	usage     llm.Usage
	spent     llm.Usage
	attempts  int
}

func (c *Config) streamVisibleStep(ctx context.Context, request []llm.Message, defs []llm.ToolDef) (result streamedStep, err error) {
	// Thinking is display-only and each attempt starts with a clean buffer.
	var thinking strings.Builder
	result.attempts, result.spent, err = c.streamStep(ctx, "LLM call", request, defs, func(text string) {
		c.emit(Event{Kind: EventTextDelta, Text: text})
	}, func(message llm.Message, usage llm.Usage) {
		result.assistant, result.usage = message, usage
		if thinking.Len() > 0 {
			c.emit(Event{Kind: EventThinkingEnd, Text: thinking.String()})
		}
	}, func(text string) {
		thinking.WriteString(text)
		c.emit(Event{Kind: EventThinkingDelta, Text: text})
	}, thinking.Reset)
	return
}

// streamTurnStep compacts and rebuilds the request if the provider reports
// a context overflow despite the local estimate.
func (c *Config) streamTurnStep(ctx context.Context, history []llm.Message, guidance *guidanceTurn, total *llm.Usage) (streamedStep, []llm.Message, []llm.Message, error) {
	const maxOverflowCompactions = 2
	for compactions := 0; ; compactions++ {
		request := c.promptView(history)
		result, err := c.streamVisibleStep(ctx, request, c.Tools)
		c.addObservedUsage(total, result.spent)
		c.emit(Event{Kind: EventStepUsage, Usage: result.spent})
		if !llm.IsContextLengthExceeded(err) || ctx.Err() != nil || c.budgetError() != nil || c.persistErr != nil || compactions >= maxOverflowCompactions {
			return result, request, history, err
		}
		var prepare *checkpointPreparation
		if guidance != nil {
			prepare = &checkpointPreparation{guidance: guidance}
		}
		compacted, usage, compactErr := c.compactMessagesPrepared(ctx, history, true, prepare)
		c.addObservedUsage(total, usage)
		c.emit(Event{Kind: EventStepUsage, Usage: usage})
		if compactErr != nil {
			return result, request, history, fmt.Errorf("emergency compaction after context overflow: %w (original: %v)", compactErr, err)
		}
		history = compacted
	}
}
