package turnbudget

import (
	"math/big"
	"testing"
)

func TestSessionBudgetsPersistAcrossSeededTurns(t *testing.T) {
	b := tracker(t, &Policy{MaxSessionTokens: number(100), MaxSessionCostUSD: dollars(1.0)})
	if err := b.SeedSessionUsage(80, big.NewRat(3, 4), true, true); err != nil {
		t.Fatal(err)
	}
	if err := b.CheckRequest(b.State().Started); err != nil {
		t.Fatal("blocked before session cap:", err)
	}
	if err := b.RecordUsage(Usage{Tokens: number(20), CostUSD: dollars(0.25)}); err != nil {
		t.Fatal("rejected exact session cap:", err)
	}
	exhausted(t, b.CheckRequest(b.State().Started), "session_tokens")
	exhausted(t, b.RecordUsage(Usage{Tokens: number(1), CostUSD: dollars(0.01)}), "session_tokens")
	if b.State().SessionReportedTokens != 101 || b.State().SessionRecordedCostUSD != 1.01 {
		t.Fatalf("unexpected session totals: %+v", b.State())
	}
	c := tracker(t, &Policy{MaxSessionCostUSD: dollars(0.5)})
	if err := c.SeedSessionUsage(10, big.NewRat(2, 5), true, true); err != nil {
		t.Fatal(err)
	}
	exhausted(t, c.RecordUsage(Usage{Tokens: number(5), CostUSD: dollars(0.2)}), "session_cost_usd")
}
