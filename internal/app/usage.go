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
		return fmt.Errorf("load usage: %w", err)
	}
	if r.usageStore != r.App.Store {
		r.usageStore = r.App.Store
		r.RunUsage, r.RunCost = llm.Usage{}, 0
	}
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
		return r.costLine(0)
	}
	return fmt.Sprintf("est. cost: ~$%.4f (rates: recorded attempt schedules; provider price unverified)", r.SessionCost)
}
