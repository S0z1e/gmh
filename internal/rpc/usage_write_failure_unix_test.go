//go:build !windows

package rpc

import (
	"context"
	"encoding/json"
	"minimalharness/internal/session"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUsageRegressionAppendFailureKeepsAccountingUnavailable(t *testing.T) {
	if os.Getenv("GMH_USAGE_EFBIG_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUsageRegressionAppendFailureKeepsAccountingUnavailable$", "-test.count=1")
		cmd.Env = append(os.Environ(), "GMH_USAGE_EFBIG_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated file-size fault: %v\n%s", err, out)
		}
		return
	}

	s, out := usageRegressionServer(t, true)
	// Keep the usage journal larger than the transcript, so only its append
	// encounters the filesystem file-size limit on the second turn.
	s.app.Cfg.UsageRates.Source = strings.Repeat("x", 10000)
	s.runTurn(Request{Message: "first"})
	if s.runner.LastErr != nil {
		t.Fatal(s.runner.LastErr)
	}
	path := filepath.Join(filepath.Dir(s.app.Store.OutputDir()), "usage.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	limit := old
	limit.Cur = uint64(info.Size())
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	s.runTurn(Request{Message: "second"})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	t.Logf("lastErr=%v metering=%+v available=%v run=%+v runCost=%v", s.runner.LastErr, s.runner.SessionMetering, s.runner.MeteringAvailable, s.runner.RunUsage, s.runner.CurrentRunCost())
	dec := json.NewDecoder(out)
	for dec.More() {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			t.Fatal(err)
		}
		if ev["type"] == "agent_settled" {
			t.Logf("settled=%+v", ev)
			if ev["metering_available"] != false {
				t.Error("lost attempt still has available lifetime metering")
			}
			if _, ok := ev["current_run_cost_usd"]; ok {
				t.Error("lost attempt has known current-run cost")
			}
			if _, ok := ev["cost_usd"]; ok {
				t.Error("lost attempt has known turn cost")
			}
		}
	}

	id := s.app.Store.ID
	if err := s.app.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.Open(id, session.OpenOptions{Workspace: s.app.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	totals, err := reopened.UsageTotals()
	if err != nil {
		t.Fatal(err)
	}
	if !totals.RecoveryGap || totals.TokenCountsComplete() {
		t.Fatalf("cold reopen erased lost attempt: %+v", totals)
	}
}
