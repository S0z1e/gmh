package turnbudget

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"
)

func number(n int64) *int64      { return &n }
func dollars(n float64) *float64 { return &n }
func boolPtr(b bool) *bool       { return &b }
func tracker(t *testing.T, p *Policy) *Tracker {
	t.Helper()
	b, err := New(p, time.Unix(1000, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func exhausted(t *testing.T, err error, resource string) {
	t.Helper()
	var e *Exceeded
	if !errors.As(err, &e) || e.Resource != resource {
		t.Fatalf("error=%v want %s exhaustion", err, resource)
	}
	if _, err := json.Marshal(e); err != nil {
		t.Fatal(err)
	}
}
func unavailable(t *testing.T, err error, resource string) {
	t.Helper()
	var e *Unavailable
	if !errors.As(err, &e) || e.Resource != resource {
		t.Fatalf("error=%v want unavailable %s", err, resource)
	}
}
func TestDefaultsExplicitZeroAndFrozenPolicy(t *testing.T) {
	defaults := tracker(t, nil).State().Limits
	if defaults.Duration != DefaultMaxDuration || defaults.ToolCalls != DefaultMaxToolCalls || defaults.ConcurrentJobs != DefaultMaxConcurrentJobs || defaults.Tokens != DefaultMaxTokens || defaults.SessionTokens != DefaultMaxSessionTokens || defaults.CostUSD != 0 || defaults.StrictMetering {
		t.Fatalf("defaults=%+v", defaults)
	}
	p := &Policy{MaxDurationSeconds: number(0), MaxToolCalls: number(0), MaxConcurrentJobs: number(0), MaxTokens: number(0), MaxSessionTokens: number(0), MaxCostUSD: dollars(0)}
	copy := p.Clone()
	*p.MaxToolCalls = 1
	*p.MaxCostUSD = 99
	b := tracker(t, copy)
	if b.State().Limits != (Limits{}) {
		t.Fatal("explicit zero did not disable caps or clone shared pointers")
	}
	if _, ok := b.Deadline(); ok {
		t.Fatal("disabled duration has deadline")
	}
	*copy.MaxToolCalls = 1
	if err := b.ReserveTools(10000); err != nil {
		t.Fatal("tracker retained mutable config:", err)
	}
	var absent *Policy
	if absent.Clone() != nil {
		t.Fatal("nil clone lost omission")
	}
}
func TestInvalidLimitsAndMissingRates(t *testing.T) {
	for _, p := range []*Policy{
		{MaxDurationSeconds: number(-1)}, {MaxToolCalls: number(-1)}, {MaxConcurrentJobs: number(-1)}, {MaxTokens: number(-1)},
		{MaxDurationSeconds: number(math.MaxInt64)}, {MaxCostUSD: dollars(-1)}, {MaxCostUSD: dollars(math.Inf(1))}, {MaxCostUSD: dollars(math.NaN())},
	} {
		if _, err := New(p, time.Now(), true); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	b, err := New(&Policy{MaxCostUSD: dollars(1)}, time.Now(), false)
	if err != nil || !b.State().BaselineIncomplete || b.State().CostComplete || b.State().CostCapEnforced || b.State().CostCapUnenforcedReason == "" {
		t.Fatalf("best-effort missing rates failed: state=%+v err=%v", b.State(), err)
	}
	if err := b.RecordUsage(Usage{Tokens: number(1)}); err != nil {
		t.Fatal("unenforced cap stopped a request:", err)
	}
	if err := b.CheckRequest(b.State().Started); err != nil || b.State().CostCapEnforced {
		t.Fatal("recorded zero looked like an enforced cap:", err, b.State().CostCapEnforced)
	}
	sessionOnly, err := New(&Policy{MaxSessionCostUSD: dollars(1)}, time.Now(), false)
	if err != nil || sessionOnly.State().SessionCostCapEnforced || sessionOnly.State().CostCapEnforced == false && sessionOnly.State().Limits.CostUSD != 0 {
		t.Fatalf("session cap enforcement=%+v err=%v", sessionOnly.State(), err)
	}
	if sessionOnly.State().CostCapEnforced != true || sessionOnly.State().SessionCostCapEnforced {
		t.Fatalf("session-only gap marked the turn cap: %+v", sessionOnly.State())
	}
	_, err = New(&Policy{MaxCostUSD: dollars(1), StrictMetering: boolPtr(true)}, time.Now(), false)
	unavailable(t, err, "cost_usd")
	_, err = New(&Policy{MaxSessionCostUSD: dollars(1), StrictMetering: boolPtr(true)}, time.Now(), false)
	unavailable(t, err, "session_cost_usd")
	if _, err := New(nil, time.Now(), false); err != nil {
		t.Fatal("disabled dollar cap required rates")
	}
}
func TestDeadlineBoundaryAndToolBatchReservation(t *testing.T) {
	b := tracker(t, &Policy{MaxDurationSeconds: number(2), MaxToolCalls: number(3)})
	deadline, ok := b.Deadline()
	if !ok {
		t.Fatal("no deadline")
	}
	if err := b.CheckDuration(deadline.Add(-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	exhausted(t, b.CheckDuration(deadline), "duration_seconds")
	if err := b.ReserveTools(2); err != nil {
		t.Fatal(err)
	}
	exhausted(t, b.ReserveTools(2), "tool_calls")
	if b.State().ToolCalls != 2 {
		t.Fatal("rejected batch consumed slots")
	}
	if err := b.ReserveTools(1); err != nil {
		t.Fatal(err)
	}
	if err := b.CheckRequest(b.State().Started); err != nil {
		t.Fatal("tool cap blocked a tool-free final answer")
	}
	exhausted(t, b.ReserveTools(1), "tool_calls")
	if err := b.ReserveTools(-1); err == nil || b.State().ToolCalls != 3 {
		t.Fatal("negative reservation refunded tools")
	}
}
func TestJobLimitUsesLiveCountRatherThanLifetimeStarts(t *testing.T) {
	b := tracker(t, &Policy{MaxConcurrentJobs: number(2)})
	if err := b.CheckJobLaunch(1); err != nil {
		t.Fatal(err)
	}
	exhausted(t, b.CheckJobLaunch(2), "concurrent_jobs")
	if err := b.CheckJobLaunch(0); err != nil {
		t.Fatal("finished jobs never freed quota")
	}
	if err := b.CheckJobLaunch(-1); err == nil {
		t.Fatal("negative running count accepted")
	}
}
func TestUsageCountsAttemptsAndStopsFutureRequestsAtExactCap(t *testing.T) {
	b := tracker(t, &Policy{MaxTokens: number(100), MaxCostUSD: dollars(1)})
	for _, u := range []Usage{{Tokens: number(30), CostUSD: dollars(.25)}, {Tokens: number(70), CostUSD: dollars(.75)}} {
		if err := b.RecordUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	s := b.State()
	if s.ReportedTokens != 100 || s.RecordedCostUSD != 1 || !s.TokensComplete || !s.CostComplete {
		t.Fatalf("state=%+v", s)
	}
	exhausted(t, b.CheckRequest(s.Started), "tokens")
	exhausted(t, b.RecordUsage(Usage{Tokens: number(1), CostUSD: dollars(.25)}), "tokens")
	if b.State().ReportedTokens != 101 || b.State().RecordedCostUSD != 1.25 {
		t.Fatal("over-budget request was refunded")
	}
	costOnly := tracker(t, &Policy{MaxCostUSD: dollars(1)})
	if err := costOnly.RecordUsage(Usage{CostUSD: dollars(1)}); err != nil {
		t.Fatal(err)
	}
	exhausted(t, costOnly.CheckRequest(costOnly.State().Started), "cost_usd")
	exhausted(t, costOnly.RecordUsage(Usage{CostUSD: dollars(.1)}), "cost_usd")
}
func TestUnknownCountersStayUnknownAndCannotBeRepairedByLaterUsage(t *testing.T) {
	for _, tc := range []struct {
		resource string
		policy   *Policy
		missing  Usage
	}{
		{"tokens", &Policy{MaxTokens: number(100), StrictMetering: boolPtr(true)}, Usage{CostUSD: dollars(.2)}},
		{"cost_usd", &Policy{MaxCostUSD: dollars(1), StrictMetering: boolPtr(true)}, Usage{Tokens: number(10)}},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			b := tracker(t, tc.policy)
			unavailable(t, b.RecordUsage(tc.missing), tc.resource)
			unavailable(t, b.RecordUsage(Usage{Tokens: number(1), CostUSD: dollars(.1)}), tc.resource)
			unavailable(t, b.CheckRequest(b.State().Started), tc.resource)
			s := b.State()
			if tc.resource == "tokens" && s.TokensComplete {
				t.Fatal("unknown tokens became complete")
			}
			if tc.resource == "cost_usd" && s.CostComplete {
				t.Fatal("unknown price became complete")
			}
		})
	}
	b := tracker(t, nil)
	if err := b.RecordUsage(Usage{}); err != nil {
		t.Fatal("disabled usage meters rejected absent data")
	}
	if b.State().TokensComplete || b.State().CostComplete {
		t.Fatal("missing usage reported as measured zero")
	}
	if err := b.CheckRequest(b.State().Started); err != nil {
		t.Fatal(err)
	}
	zero := tracker(t, &Policy{MaxTokens: number(1), MaxCostUSD: dollars(1)})
	if err := zero.RecordUsage(Usage{Tokens: number(0), CostUSD: dollars(0)}); err != nil {
		t.Fatal(err)
	}
	if !zero.State().TokensComplete || !zero.State().CostComplete {
		t.Fatal("explicit zero treated as missing")
	}
}
func TestInvalidUsageCannotRefundOrEraseOtherConsumption(t *testing.T) {
	b := tracker(t, nil)
	if err := b.RecordUsage(Usage{Tokens: number(10), CostUSD: dollars(.5)}); err != nil {
		t.Fatal(err)
	}
	unavailable(t, b.RecordUsage(Usage{Tokens: number(-10), CostUSD: dollars(.5)}), "tokens")
	s := b.State()
	if s.ReportedTokens != 10 || s.RecordedCostUSD != 1 || s.TokensComplete {
		t.Fatalf("bad usage erased consumption: %+v", s)
	}
	disabled := tracker(t, &Policy{MaxTokens: number(0), MaxSessionTokens: number(0)})
	if err := disabled.RecordUsage(Usage{Tokens: number(-10)}); err != nil || disabled.State().TokensComplete || !disabled.State().LowerBound {
		t.Fatalf("disabled token caps stopped on a quarantined report: %+v %v", disabled.State(), err)
	}
	for _, bad := range []float64{-1, math.Inf(1), math.NaN()} {
		c := tracker(t, nil)
		if err := c.RecordUsage(Usage{Tokens: number(3), CostUSD: dollars(bad)}); err != nil || c.State().ReportedTokens != 3 || c.State().CostComplete {
			t.Fatalf("invalid price without a dollar cap stopped the meter: %+v %v", c.State(), err)
		}
		priced := tracker(t, &Policy{MaxCostUSD: dollars(1)})
		unavailable(t, priced.RecordUsage(Usage{Tokens: number(3), CostUSD: dollars(bad)}), "cost_usd")
	}
	c := tracker(t, &Policy{MaxToolCalls: number(0)})
	if err := c.ReserveTools(math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if err := c.ReserveTools(1); err == nil || c.State().ToolCalls != math.MaxInt64 {
		t.Fatal("tool count overflowed")
	}
	d := tracker(t, &Policy{MaxTokens: number(0), MaxSessionTokens: number(0)})
	if err := d.RecordUsage(Usage{Tokens: number(math.MaxInt64), CostUSD: dollars(math.MaxFloat64)}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordUsage(Usage{Tokens: number(1), CostUSD: dollars(math.MaxFloat64)}); err != nil || d.State().TokensComplete || d.State().CostComplete || !d.State().LowerBound {
		t.Fatalf("disabled caps treated overflow as a stop: %+v %v", d.State(), err)
	}
}

func TestDecimalCostBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cap   float64
		costs []float64
	}{
		{"tenths", 1, []float64{.1, .1, .1, .1, .1, .1, .1, .1, .1, .1}},
		{"mixed", .3, []float64{.1, .2}},
		{"tiny", 3e-20, []float64{1e-20, 2e-20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tracker(t, &Policy{MaxCostUSD: dollars(tc.cap)})
			for _, cost := range tc.costs {
				if err := b.CheckRequest(b.State().Started); err != nil {
					t.Fatal("stopped before cap:", err)
				}
				if err := b.RecordUsage(Usage{CostUSD: dollars(cost)}); err != nil {
					t.Fatal("rejected exact-cap response:", err)
				}
			}
			if b.State().RecordedCostUSD != tc.cap {
				t.Fatalf("cost=%g want %g", b.State().RecordedCostUSD, tc.cap)
			}
			exhausted(t, b.CheckRequest(b.State().Started), "cost_usd")
			exhausted(t, b.RecordUsage(Usage{CostUSD: dollars(math.SmallestNonzeroFloat64)}), "cost_usd")
		})
	}
}

func TestIncompleteUsageStillStopsAtTheRecordedCost(t *testing.T) {
	b := tracker(t, &Policy{MaxCostUSD: dollars(1)})
	if err := b.RecordUsage(Usage{CostUSD: dollars(.25)}); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordUsage(Usage{Tokens: number(1)}); err != nil || !b.State().LowerBound || !b.State().CostCapEnforced {
		t.Fatalf("a missing price disabled a schedule that can still grow: %+v %v", b.State(), err)
	}
	exhausted(t, b.RecordUsage(Usage{CostUSD: dollars(1)}), "cost_usd")
	session := tracker(t, &Policy{MaxSessionCostUSD: dollars(1)})
	if err := session.SeedSessionUsage(0, big.NewRat(3, 4), true, false); err != nil {
		t.Fatal(err)
	}
	exhausted(t, session.RecordUsage(Usage{CostUSD: dollars(.5)}), "session_cost_usd")
}

func TestExactPriceAmountsAreCopiedAndNeverRoundedForEnforcement(t *testing.T) {
	b := tracker(t, &Policy{MaxCostUSD: dollars(.11)})
	cost := big.NewRat(11, 100)
	if err := b.RecordUsage(Usage{ExactCostUSD: cost}); err != nil {
		t.Fatal(err)
	}
	cost.SetInt64(100)
	if b.State().RecordedCostUSD != .11 {
		t.Fatal("caller mutation changed recorded cost")
	}
	exhausted(t, b.CheckRequest(b.State().Started), "cost_usd")
	tiny, _ := new(big.Rat).SetString("1e-300")
	exhausted(t, b.RecordUsage(Usage{ExactCostUSD: tiny}), "cost_usd")
	for _, u := range []Usage{{ExactCostUSD: big.NewRat(-1, 10)}, {CostUSD: dollars(1), ExactCostUSD: big.NewRat(1, 1)}} {
		c := tracker(t, &Policy{MaxCostUSD: dollars(1)})
		unavailable(t, c.RecordUsage(u), "cost_usd")
		if c.State().CostComplete || c.State().RecordedCostUSD != 0 {
			t.Fatal("invalid cost accepted")
		}
	}
}
