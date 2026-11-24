package app

import (
	"math/big"
	"testing"
	"time"

	"minimalharness/internal/config"
	"minimalharness/internal/llm"
)

func floatPtr(v float64) *float64 { return &v }

func TestRatesForModelMergesEnvOverridesIntoTiers(t *testing.T) {
	mdl := config.Model{
		ID: "claude-sonnet",
		Cost: &config.ModelCost{
			Input:      floatPtr(3),
			Output:     floatPtr(15),
			CacheRead:  floatPtr(0.3),
			CacheWrite: floatPtr(3.75),
			Tiers: []config.CostTier{{
				InputTokensAbove: floatPtr(200000),
				Input:            floatPtr(6),
				Output:           floatPtr(22.5),
				CacheRead:        floatPtr(0.6),
				CacheWrite:       floatPtr(7.5),
			}},
		},
	}
	envRates := llm.Rates{
		InputPerMTok:     4,
		InputConfigured:  true,
		PeakInputPerMTok: 8,
		Configured:       true,
		Source:           "env override",
	}
	merged := ratesForModel(envRates, "anthropic", mdl)
	if !merged.CacheWriteConfigured || merged.CacheWritePerMTok != 3.75 {
		t.Fatalf("lost model cacheWrite rate: %+v", merged)
	}
	if merged.InputPerMTok != 4 || merged.OutputPerMTok != 15 {
		t.Fatalf("unexpected base rates: %+v", merged)
	}
	if len(merged.Tiers) != 1 || merged.Tiers[0].InputPerMTok != 4 || merged.Tiers[0].OutputPerMTok != 22.5 {
		t.Fatalf("env override did not propagate to tier: %+v", merged.Tiers)
	}
	// Above tier threshold (300k prompt tokens), env InputPerMTok (4) must apply,
	// while non-overridden tier OutputPerMTok (22.5) still applies.
	u := llm.Usage{PromptTokens: 300000, CompletionTokens: 100000, TotalTokens: 400000, CacheUsageReported: true}
	peak := time.Date(2026, time.November, 24, 2, 0, 0, 0, time.UTC)
	cost, ok := merged.BudgetCost(u, peak.Add(-2*time.Hour))
	if !ok {
		t.Fatal("expected BudgetCost to succeed")
	}
	got, _ := cost.Float64()
	// 0.3 * 4 + 0.1 * 22.5 = 1.2 + 2.25 = 3.45
	if got != 3.45 {
		t.Fatalf("cost=%g want 3.45", got)
	}
	cost, ok = merged.BudgetCost(u, peak)
	if !ok || cost.Cmp(big.NewRat(465, 100)) != 0 {
		t.Fatalf("peak cost=%v want 4.65", cost)
	}
}
