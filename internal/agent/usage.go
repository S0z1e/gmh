package agent

import (
	"context"
	"fmt"
	"math"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

// Accounting runs even when request diagnostics are disabled. Persist exactly
// the observed attempt before another request or tool action can begin.
func (c *Config) recordAttempt(operation string, attempt int, trace llm.RequestTrace) {
	c.usageAttempts++
	invalid := trace.Usage != nil && !trace.Usage.Valid()
	if invalid && c.persistErr == nil {
		c.persistErr = fmt.Errorf("invalid provider usage counters")
	}
	if !trace.RejectedBeforeInference() {
		c.recordBudgetUsage(trace.Usage, trace.StartedAt, trace.TokensComplete, trace.PriceCountsComplete)
	}
	if c.Session == nil {
		return
	}
	record := session.UsageAttempt{ID: trace.ID, Operation: operation, Attempt: attempt, Request: trace}
	if trace.Usage != nil && !invalid && trace.PriceCountsComplete && trace.Usage.TotalTokens == trace.Usage.PromptTokens+trace.Usage.CompletionTokens && c.UsageRates.Configured {
		if amount, known := c.UsageRates.BudgetCost(*trace.Usage, trace.StartedAt); known {
			estimate, _ := amount.Float64()
			if !math.IsNaN(estimate) && !math.IsInf(estimate, 0) && estimate >= 0 {
				record.EstimateUSD, record.RateSource = &estimate, c.UsageRates.Source
				record.ExactEstimateUSD = amount.RatString()
			}
		}
	}
	if err := c.Session.RecordUsage(record); err != nil && c.persistErr == nil {
		c.persistErr = fmt.Errorf("save provider usage: %w", err)
	}
}

// Preserve a valid measured subtotal if provider counters exceed the range of
// a turn aggregate. The ledger separately records the attempt's coverage gap.
func (c *Config) addObservedUsage(total *llm.Usage, usage llm.Usage) {
	if total.CanAdd(usage) {
		total.Add(usage)
		return
	}
	if c.persistErr == nil {
		c.persistErr = fmt.Errorf("invalid or overflowing turn usage")
	}
}

func (c *Config) requestContext(ctx context.Context, operation string, attempt int, spent *llm.Usage) context.Context {
	requestCtx := llm.WithRequestObserver(ctx, func(trace llm.RequestTrace) {
		if trace.Usage != nil && trace.Usage.Valid() && spent.CanAdd(*trace.Usage) {
			spent.Add(*trace.Usage)
		} else if trace.Usage != nil && c.persistErr == nil {
			c.persistErr = fmt.Errorf("invalid or overflowing provider usage")
		}
		c.recordAttempt(operation, attempt, trace)
		if c.TraceRequests {
			c.emit(Event{Kind: EventLLMRequest, Request: &trace, Operation: operation, Attempt: attempt})
			if c.Session != nil {
				_ = c.Session.Audit(struct {
					Kind      string           `json:"kind"`
					Operation string           `json:"operation"`
					Attempt   int              `json:"attempt"`
					Request   llm.RequestTrace `json:"request"`
				}{"llm_request", operation, attempt, trace})
			}
		}
	})
	requestCtx = llm.WithRequestStart(requestCtx, func(trace llm.RequestTrace) error {
		if c.Session == nil {
			return nil
		}
		if err := c.Session.BeginUsageAttempt(trace.ID); err != nil {
			c.persistErr = fmt.Errorf("save pending provider usage attempt: %w", err)
			return c.persistErr
		}
		return nil
	})
	return requestCtx
}
