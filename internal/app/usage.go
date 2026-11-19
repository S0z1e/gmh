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
	return nil
}

func (r *Runner) accountRun(usage llm.Usage, previousCost float64) float64 {
	if err := r.RefreshUsage(); err != nil {
		r.LastErr = err
		r.Print("[error] %v", err)
	}
	cost := r.SessionCost - previousCost
	r.RunUsage.Add(usage)
	r.RunCost += cost
	return cost
}

func (r *Runner) usageCoverage() string {
	t := r.SessionMetering
	return fmt.Sprintf("attempts=%d unreported=%d partial=%d priced=%d legacy_history=%t", t.Attempts, t.UnreportedAttempts, t.PartialAttempts, t.PricedAttempts, t.LegacyHistory)
}

func (r *Runner) sessionCostLine() string {
	if r.SessionMetering.PricedAttempts == 0 {
		return "fee: " + r.App.Cfg.Client.ProviderName() + " price unverified"
	}
	if r.SessionMetering.PricedAttempts != r.SessionMetering.Attempts || r.SessionMetering.LegacyHistory {
		return fmt.Sprintf("est. recorded subtotal: ~$%.4f (pricing incomplete; provider price unverified)", r.SessionCost)
	}
	return fmt.Sprintf("est. cost: ~$%.4f (rates: recorded attempt schedules; provider price unverified)", r.SessionCost)
}

// A nil cost means at least one attempt in this run could not be priced.
// An empty run and explicitly configured free rates both have a known zero.
func (r *Runner) CurrentRunCost() *float64 {
	if !r.MeteringAvailable {
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
