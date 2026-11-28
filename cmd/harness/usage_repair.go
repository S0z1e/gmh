package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"minimalharness/internal/session"
)

// usageRepair quarantines one named usage artifact or accepts a lower-bound
// baseline and prints the artifacts/gaps that still block strict metering.
// Quarantining backups and pending markers is idempotent when already gone;
// quarantining a healthy usage journal is refused unless --allow-healthy is set.
func usageRepair(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("usage-repair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sessionID := fs.String("session", "", "session id whose usage journal is repaired")
	root := fs.String("root", ".", "workspace root bound to the session")
	artifact := fs.String("quarantine", "", "artifact path named by the strict metering error")
	allowHealthy := fs.Bool("allow-healthy", false, "allow quarantining a usage journal that loads cleanly (audited)")
	acceptLowerBound := fs.Bool("accept-lower-bound", false, "acknowledge an incomplete or quarantined lower-bound baseline so strict metering enforces caps against recorded totals (audited)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionID == "" || (*artifact == "" && !*acceptLowerBound) {
		return fmt.Errorf("usage-repair requires --session and --quarantine or --accept-lower-bound")
	}
	store, err := session.Open(*sessionID, session.OpenOptions{Workspace: *root, Existing: true, RepairDamagedUsage: true})
	if err != nil {
		return err
	}
	defer store.Close()
	if *artifact != "" {
		report, err := store.RepairUsageWithOptions(session.UsageRepairQuarantine, *artifact, session.UsageRepairOptions{
			ForceHealthy: *allowHealthy,
		})
		if err != nil {
			return err
		}
		if report.HealthyOverride {
			fmt.Fprintf(stdout, "quarantined %s (healthy override audited)\n", *artifact)
		} else {
			fmt.Fprintf(stdout, "quarantined %s\n", *artifact)
		}
		if report.CarryForward != nil {
			fmt.Fprintf(stdout, "carried forward %d attempt(s), %d tokens ($%.4f) to %s\n", report.CarryForward.Attempts, report.CarryForward.Usage.TotalTokens, report.CarryForward.EstimateUSD, report.CarryForward.QuarantinedFile)
		}
	}
	if *acceptLowerBound {
		_, changed, err := store.AcceptUsageLowerBound()
		if err != nil {
			return err
		}
		if changed {
			fmt.Fprintf(stdout, "accepted lower-bound baseline for session %s\n", *sessionID)
		} else {
			fmt.Fprintf(stdout, "lower-bound baseline already accepted or complete for session %s\n", *sessionID)
		}
	}
	recovery, err := store.UsageRecovery()
	if err != nil {
		return err
	}
	totals, err := store.UsageTotals()
	if err != nil {
		return err
	}
	if len(recovery.Artifacts) > 0 {
		fmt.Fprintln(stdout, "still blocking strict metering:")
		for _, item := range recovery.Artifacts {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", item.Path, item.Kind, strings.ReplaceAll(item.Reason, "\n", " "))
		}
		return nil
	}
	if !totals.TokenCountsComplete() && !totals.LowerBoundTokensAccepted() {
		fmt.Fprintf(stdout, "strict metering still blocked: %s\n", session.IncompleteUsageError(*sessionID, *root, totals))
		return nil
	}
	if !totals.TokenCountsComplete() {
		fmt.Fprintln(stdout, "strict metering unblocked with lower-bound baseline")
		return nil
	}
	fmt.Fprintln(stdout, "recovery gap cleared")
	if totals.UnpricedAttempts() > 0 && !totals.LowerBoundCostAccepted() {
		fmt.Fprintf(stdout, "note: %d unpriced attempt(s) remain if a strict session cost cap is active (acknowledge with --accept-lower-bound)\n", totals.UnpricedAttempts())
	}
	return nil
}

func runUsageRepair(args []string) {
	if err := usageRepair(args, os.Stdout, os.Stderr); err != nil {
		fatal(err)
	}
}
