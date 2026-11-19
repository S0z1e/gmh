package app

import (
	"fmt"
	"minimalharness/internal/llm"
)

// RefreshUsage restores lifetime totals without changing the cost of past
// attempts when models/rates change. Run totals start at writer acquisition.
func (r *Runner) RefreshUsage() error {
	if r.App == nil || r.App.Store == nil {
		return nil
	}
	totals, err := r.App.Store.UsageTotals()
	if err != nil {
		r.MeteringAvailable = false
		r.runAccountingIncomplete = true
		return fmt.Errorf("load usage: %w", err)
	}
	if r.usageStore != r.App.Store {
		r.usageStore = r.App.Store
		r.RunUsage, r.RunCost = llm.Usage{}, 0
		r.runBaseline = totals
		r.runAccountingIncomplete = false
	}
	r.MeteringAvailable = !r.runAccountingIncomplete
	r.SessionMetering = totals
	r.SessionUsage, r.SessionCost = totals.Usage, totals.EstimateUSD
	if r.runAccountingIncomplete {
		return fmt.Errorf("usage accounting is incomplete for the current run")
	}
	return nil
}

func (r *Runner) accountRun(usage llm.Usage, previousCost float64) float64 {
	if err := r.RefreshUsage(); err != nil {
		r.LastErr = err
		r.Print("[error] %v", err)
	}
	cost := r.SessionCost - previousCost
	if r.RunUsage.CanAdd(usage) {
		r.RunUsage.Add(usage)
	} else {
		r.runAccountingIncomplete = true
		r.MeteringAvailable = false
		r.LastErr = fmt.Errorf("current-run usage is invalid or exceeds the accounting range")
		r.Print("[error] %v", r.LastErr)
	}
	r.RunCost += cost
	return cost
}

func (r *Runner) usageCoverage() string {
	t := r.SessionMetering
	return fmt.Sprintf("attempts=%d unreported=%d partial=%d invalid=%d priced=%d legacy_history=%t recovery_gap=%t", t.Attempts, t.UnreportedAttempts, t.PartialAttempts, t.InvalidAttempts, t.PricedAttempts, t.LegacyHistory, t.RecoveryGap)
}

func (r *Runner) sessionCostLine() string {
	if r.SessionMetering.PricedAttempts == 0 {
		return "fee: " + r.App.Cfg.Client.ProviderName() + " price unverified"
	}
	if r.SessionMetering.PricedAttempts != r.SessionMetering.Attempts || (r.SessionMetering.LegacyHistory || r.SessionMetering.RecoveryGap) {
		return fmt.Sprintf("est. recorded subtotal: ~$%.4f (pricing incomplete; provider price unverified)", r.SessionCost)
	}
	return fmt.Sprintf("est. cost: ~$%.4f (rates: recorded attempt schedules; provider price unverified)", r.SessionCost)
}

// A nil cost means at least one attempt in this run could not be priced.
// An empty run and explicitly configured free rates both have a known zero.
func (r *Runner) CurrentRunCost() *float64 {
	if !r.MeteringAvailable || r.SessionMetering.RecoveryGap && !r.runBaseline.RecoveryGap {
		return nil
	}
	attempts := r.SessionMetering.Attempts - r.runBaseline.Attempts
	priced := r.SessionMetering.PricedAttempts - r.runBaseline.PricedAttempts
	if attempts != priced {
		return nil
	}
	cost := r.RunCost
	return &cost
}
func (r *Runner) runCostLine() string {
	if r.CurrentRunCost() == nil {
		return "fee: provider price unverified (current-run pricing incomplete)"
	}
	return r.costLine(r.RunCost)
}

func (r *Runner) SessionUsageComplete() bool {
	return r.MeteringAvailable && r.SessionMetering.TokenCountsComplete()
}
func (r *Runner) sessionUsageLine() string {
	if !r.MeteringAvailable {
		return "unavailable (accounting error)"
	}
	if !r.SessionMetering.TokenCountsComplete() {
		return fmt.Sprintf("%s (recorded subtotal; usage incomplete: %s)", r.SessionUsage, r.usageCoverage())
	}
	return r.SessionUsage.String()
}
