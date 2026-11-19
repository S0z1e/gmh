package agent

import (
	"fmt"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

// Accounting runs even when request diagnostics are disabled. Persist exactly
// the observed attempt before another request or tool action can begin.
func (c *Config) recordAttempt(operation string, attempt int, trace llm.RequestTrace) {
	c.usageAttempts++
	c.recordBudgetUsage(trace.Usage, trace.StartedAt, trace.TokensComplete, trace.PriceCountsComplete)
	if c.Session == nil {
		return
	}
	record := session.UsageAttempt{ID: trace.ID, Operation: operation, Attempt: attempt, Request: trace}
	if trace.Usage != nil && trace.PriceCountsComplete && c.UsageRates.Configured {
		estimate := c.UsageRates.Cost(*trace.Usage, trace.StartedAt)
		record.EstimateUSD, record.RateSource = &estimate, c.UsageRates.Source
	}
	if err := c.Session.RecordUsage(record); err != nil && c.persistErr == nil {
		c.persistErr = fmt.Errorf("save provider usage: %w", err)
	}
}
