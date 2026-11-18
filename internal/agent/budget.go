package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/turnbudget"
)

type turnResources struct {
	attempts          int64
	api               string
	rates             llm.Rates
	tracker           *turnbudget.Tracker
	parent            context.Context
	failure           error
	recorded, emitted bool
	settled           bool
	text              string
}

func (c *Config) startBudget(parent context.Context) (context.Context, context.CancelFunc, error) {
	c.LastTurnBudget = nil
	api := ""
	if c.Client != nil {
		api = c.Client.Mode()
	}
	c.resources = &turnResources{parent: parent, rates: c.UsageRates, api: api}
	b, err := turnbudget.New(c.TurnBudget, time.Now(), c.UsageRates.BudgetReady() && api != "anthropic")
	if err != nil {
		c.resources.failure = err
		return parent, func() {}, err
	}
	c.resources.tracker = b
	if deadline, ok := b.Deadline(); ok {
		ctx, cancel := context.WithDeadline(parent, deadline)
		return ctx, cancel, nil
	}
	return parent, func() {}, nil
}

// budgetError checks exhaustion, not equality: an exact-cap final response is
// allowed. CheckRequest separately forbids another attempt at equality.
func (c *Config) budgetError() error {
	r := c.resources
	if r == nil || r.parent.Err() != nil {
		return nil
	}
	if r.tracker != nil {
		if err := r.tracker.CheckDuration(time.Now()); err != nil {
			r.failure = err
		}
	}
	return r.failure
}

func (c *Config) budgetRequest() error {
	if err := c.budgetError(); err != nil {
		return err
	}
	if r := c.resources; r != nil && r.tracker != nil {
		if err := r.tracker.CheckRequest(time.Now()); err != nil {
			r.failure = err
			return err
		}
	}
	return nil
}

func (c *Config) recordBudgetUsage(usage *llm.Usage, started time.Time, tokensComplete, priceCountsComplete bool) {
	r := c.resources
	if r == nil || r.tracker == nil {
		return
	}
	r.attempts++
	var measured turnbudget.Usage
	if usage != nil {
		u := *usage
		tokens := int64(u.TotalTokens)
		if u.PromptTokens < 0 || u.CompletionTokens < 0 || tokens < 0 || int64(u.CompletionTokens) > math.MaxInt64-int64(u.PromptTokens) {
			tokens = -1
		} else if sum := int64(u.PromptTokens) + int64(u.CompletionTokens); tokens < sum {
			tokens = sum
		}
		measured.Tokens = &tokens
		measured.TokensPartial = !tokensComplete
		if priceCountsComplete && r.api != "anthropic" && tokens >= 0 && tokens == int64(u.PromptTokens)+int64(u.CompletionTokens) {
			if cost, known := r.rates.BudgetCost(u, started); known {
				measured.ExactCostUSD = cost
			}
		}
	}
	if err := r.tracker.RecordUsage(measured); err != nil && r.failure == nil {
		r.failure = err
	}
}

func (c *Config) budgetTerminal(e Event) Event {
	err := c.budgetError()
	if c.resources != nil {
		// Settlement precedes persistence and the synchronous event callback.
		// Time spent publishing this outcome cannot change the settled result.
		c.resources.settled = true
	}
	if err == nil || c.persistErr != nil {
		return e
	}
	r := c.resources
	reason := TerminalError
	var exceeded *turnbudget.Exceeded
	var unavailable *turnbudget.Unavailable
	if errors.As(err, &exceeded) {
		reason = TerminalBudgetExceeded
	}
	if errors.As(err, &unavailable) {
		reason = TerminalBudgetUnavailable
	}
	if !r.recorded {
		r.recorded = true
		var state *turnbudget.State
		if r.tracker != nil {
			s := r.tracker.State()
			state = &s
		}
		data, marshalErr := json.Marshal(struct {
			Outcome TaskOutcome       `json:"outcome"`
			Reason  TerminalReason    `json:"reason"`
			Error   string            `json:"error"`
			Budget  *turnbudget.State `json:"budget,omitempty"`
		}{OutcomeIncomplete, reason, err.Error(), state})
		if marshalErr != nil {
			c.persistErr = fmt.Errorf("encode budget outcome: %w", marshalErr)
		} else {
			c.persist(llm.Message{Role: "user", Kind: "task_outcome", Content: "[harness task outcome]\n" + string(data)})
		}
	}
	if c.persistErr != nil {
		err, reason = c.persistErr, TerminalError
	}
	text := fmt.Sprintf("Task incomplete: %v. Completed actions have not been rolled back; check the current state before resuming.", err)
	if strings.TrimSpace(e.Text) != "" {
		text += "\n\nLast response:\n" + e.Text
	}
	r.emitted, r.text = true, text
	c.LastTerminalReason = reason
	e.Kind, e.Err, e.Text, e.TerminalReason, e.TaskOutcome = EventError, err, text, reason, OutcomeIncomplete
	return e
}

func (c *Config) finishBudget(text *string, runErr *error) {
	r := c.resources
	if r == nil {
		return
	}
	defer func() { c.resources = nil }()
	if !r.settled {
		if err := c.budgetError(); err != nil {
			c.emit(Event{Kind: EventError, Err: err, Text: *text, TerminalReason: TerminalError})
		}
	}
	if r.emitted {
		*text, *runErr = r.text, r.failure
	}
	if r.tracker != nil {
		s := r.tracker.State()
		c.LastTurnBudget = &s
	}
}

func (c *Config) reserveBudgetTools(calls []llm.ToolCall) error {
	if r := c.resources; r != nil && r.tracker != nil {
		if err := r.tracker.ReserveTools(int64(len(calls))); err != nil {
			r.failure = err
			return err
		}
	}
	return c.budgetError()
}

const notExecutedKind = "not_executed"

// Calls rejected before dispatch have no committed invocation ID. Store a real
// tool result for each so resume never treats them as interrupted side effects.
func (c *Config) rejectBudgetTools(calls []llm.ToolCall, err error, startedInvocation ...string) []llm.Message {
	var results []llm.Message
	for i, tc := range calls {
		obs := fmt.Sprintf("Not executed: %v", err)
		invocation := ""
		if i == 0 && len(startedInvocation) > 0 {
			invocation = startedInvocation[0]
		} else {
			c.emit(Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments, ToolCallID: tc.ID})
		}
		c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, InvocationID: invocation, ToolOutput: obs, ToolErr: err})
		result := llm.Message{Role: "tool", Kind: notExecutedKind, ToolCallID: tc.ID, InvocationID: invocation, ToolError: true, Content: obs}
		results = append(results, result)
		c.persist(result)
	}
	return results
}
