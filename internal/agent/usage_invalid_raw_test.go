package agent

import (
	"context"
	"fmt"
	"math"
	"minimalharness/internal/llm"
	"testing"
)

func TestUsageRegressionAnthropicInvalidRawCounters(t *testing.T) {
	for _, tc := range []struct {
		name            string
		input, creation int
	}{
		{"negative_creation", 10, -1},
		{"wrapped_prompt", math.MaxInt, math.MaxInt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached := 0
			if tc.name == "wrapped_prompt" {
				cached = 3
			}
			body := fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d,\"cache_creation_input_tokens\":%d,\"cache_read_input_tokens\":%d}}}\n\n", tc.input, tc.creation, cached) +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"answer\"}}\n\n" +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
			c, s := usageRegressionConfig(t, body)
			c.Client.API = "anthropic"
			_, spent, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "hello"}})
			totals, loadErr := s.UsageTotals()
			t.Logf("spent=%+v err=%v totals=%+v loadErr=%v", spent, err, totals, loadErr)
			if err == nil || totals.InvalidAttempts != 1 || totals.Usage.TotalTokens != 0 {
				t.Error("invalid raw usage accepted as valid metering")
			}
		})
	}
}
