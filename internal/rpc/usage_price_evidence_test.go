package rpc

import (
	"encoding/json"
	"minimalharness/internal/llm"
	"testing"
)

func TestUsageRegressionIncompletePriceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rates llm.Rates
	}{
		{"omitted_output_rate", llm.Rates{Configured: true, InputPerMTok: 1000, Source: "input only"}},
		{"unknown_cache_at_discount", llm.Rates{Configured: true, Complete: true, InputPerMTok: 1000, OutputPerMTok: 2000, CachedPerMTok: 100, Source: "complete schedule"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, out := usageRegressionServer(t, false)
			s.app.Rates = tc.rates
			s.app.Cfg.UsageRates = tc.rates
			s.runTurn(Request{Message: "hello"})
			out.Reset()
			s.handleQuery(Request{Type: "usage"})
			var r map[string]any
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			t.Logf("response=%s", out.Bytes())
			d := r["data"].(map[string]any)
			if _, ok := d["current_run_cost_usd"]; ok {
				t.Error("incomplete evidence published as fully known cost")
			}
			if s.runner.SessionMetering.PricedAttempts != 0 {
				t.Error("incomplete attempt marked fully priced in durable ledger")
			}
		})
	}
}
