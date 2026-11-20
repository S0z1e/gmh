package agent

import (
	"context"
	"minimalharness/internal/llm"
	"testing"
)

func TestUsageInvalidSnapshotCannotBecomeValid(t *testing.T) {
	for _, final := range []string{`{}`, `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`} {
		t.Run(final, func(t *testing.T) {
			body := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens_details\":{\"reasoning_tokens\":-1}}}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":" + final + "}\n\ndata: [DONE]\n\n"
			c, s := usageRegressionConfig(t, body)
			_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
			totals, loadErr := s.UsageTotals()
			if err == nil || loadErr != nil || totals.InvalidAttempts != 1 || totals.PricedAttempts != 0 {
				t.Fatalf("invalid snapshot erased: %+v err=%v loadErr=%v", totals, err, loadErr)
			}
		})
	}
}

func TestUsageDecodeFailureInvalidatesPriorSnapshot(t *testing.T) {
	for _, counter := range []string{`9223372036854775808`, `"wrong type"`} {
		t.Run(counter, func(t *testing.T) {
			body := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
				"data: {\"usage\":{\"prompt_tokens\":" + counter + "}}\n\n"
			c, store := usageRegressionConfig(t, body)
			c.UsageRates = llm.Rates{Configured: true, Complete: true, InputPerMTok: 1000, CachedPerMTok: 1000, OutputPerMTok: 2000}
			_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
			totals, loadErr := store.UsageTotals()
			if err == nil || loadErr != nil || totals.InvalidAttempts != 1 || totals.PricedAttempts != 0 || totals.TokenCountsComplete() {
				t.Fatalf("decode failure erased evidence: %+v err=%v loadErr=%v", totals, err, loadErr)
			}
		})
	}
}
