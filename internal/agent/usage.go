package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

// Accounting runs even when request diagnostics are disabled. Persist exactly
// the observed attempt before another request or tool action can begin.
func (c *Config) recordAttempt(operation string, attempt int, trace llm.RequestTrace) {
	c.usageAttempts++
	invalid := trace.Usage != nil && !trace.Usage.Valid()
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
		if errors.Is(err, session.ErrInvalidProviderUsage) && !c.invalidUsageStopsTurn() {
			fmt.Fprintf(os.Stderr, "warning: provider usage was invalid and was recorded as unknown; the turn continues because no active cap requires complete evidence\n")
		} else {
			c.persistErr = fmt.Errorf("save provider usage: %w", err)
		}
	}
}

// invalidUsageStopsTurn keeps a malformed provider report from ending a
// non-strict turn whose caps are all disabled. Defaults still include token
// caps, and those need complete evidence. Strict mode always stops.
func (c *Config) invalidUsageStopsTurn() bool {
	if c.resources == nil || c.resources.tracker == nil {
		return true
	}
	limits := c.resources.tracker.State().Limits
	if limits.StrictMetering {
		return true
	}
	return limits.Tokens > 0 || limits.SessionTokens > 0 || limits.CostUSD > 0 || limits.SessionCostUSD > 0
}

func isQuarantinedProviderUsage(err error) bool {
	return errors.Is(err, session.ErrInvalidProviderUsage)
}

func persistErrIsQuarantine(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return isQuarantinedProviderUsage(err) || strings.Contains(text, session.ErrInvalidProviderUsage.Error()) || strings.Contains(text, "invalid or overflowing provider usage") || strings.Contains(text, "invalid provider usage counters")
}

// Preserve a valid measured subtotal if provider counters exceed the range of
// a turn aggregate. The ledger separately records the attempt's coverage gap.
func (c *Config) addObservedUsage(total *llm.Usage, usage llm.Usage) {
	if total.CanAdd(usage) {
		total.Add(usage)
		return
	}
	if c.persistErr == nil && c.invalidUsageStopsTurn() {
		c.persistErr = fmt.Errorf("invalid or overflowing turn usage")
	}
}

func (c *Config) requestContext(ctx context.Context, operation string, attempt int, spent *llm.Usage) context.Context {
	requestCtx := llm.WithRequestObserver(ctx, func(trace llm.RequestTrace) {
		if trace.Usage != nil && trace.Usage.Valid() && spent.CanAdd(*trace.Usage) {
			spent.Add(*trace.Usage)
		} else if trace.Usage != nil && !trace.Usage.Valid() && c.persistErr == nil && c.invalidUsageStopsTurn() {
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
	// A gate failure happens before HTTP. Record that explicitly so the marker
	// can later be reconciled as not-dispatched instead of unknown usage.
	requestCtx = llm.WithRequestAbort(requestCtx, func(id string) {
		if c.Session == nil || id == "" {
			return
		}
		_ = c.Session.MarkUsageNotDispatched(id)
	})
	return requestCtx
}
