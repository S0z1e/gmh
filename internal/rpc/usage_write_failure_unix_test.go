//go:build !windows

package rpc

import (
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestUsageRegressionAppendFailureKeepsAccountingUnavailable(t *testing.T) {
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
}
