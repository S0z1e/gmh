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
