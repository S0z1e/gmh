package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"minimalharness/internal/session"
)

// usageRepair quarantines one named usage artifact and prints the artifacts
// that still block strict metering. It is idempotent: quarantining an artifact
// that is already gone reports the remaining set instead of deleting evidence.
func usageRepair(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("usage-repair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sessionID := fs.String("session", "", "session id whose usage journal is repaired")
	root := fs.String("root", ".", "workspace root bound to the session")
	artifact := fs.String("quarantine", "", "artifact path named by the strict metering error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionID == "" || *artifact == "" {
		return fmt.Errorf("usage-repair requires --session and --quarantine")
	}
	store, err := session.Open(*sessionID, session.OpenOptions{Workspace: *root, Existing: true, RepairDamagedUsage: true})
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := store.RepairUsage(session.UsageRepairQuarantine, *artifact)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "quarantined %s\n", *artifact)
	if len(report.Artifacts) == 0 {
		fmt.Fprintln(stdout, "recovery gap cleared")
		return nil
	}
	fmt.Fprintln(stdout, "still blocking strict metering:")
	for _, item := range report.Artifacts {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", item.Path, item.Kind, strings.ReplaceAll(item.Reason, "\n", " "))
	}
	return nil
}

func runUsageRepair(args []string) {
	if err := usageRepair(args, os.Stdout, os.Stderr); err != nil {
		fatal(err)
	}
}
