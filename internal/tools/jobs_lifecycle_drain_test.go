package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCloseJobsPreservesExitedShellDuringDrain(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	e.SwitchSession("owner")
	job, err := e.BashBackground(context.Background(), BashArgs{Command: "sleep 30 & printf ready > ready; exit 7", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer job.Cancel()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(filepath.Join(e.Root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not ready")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(40 * time.Millisecond)
	if _, done := job.Result(); done {
		t.Fatal("not in output drain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.CloseJobs(ctx); err != nil {
		t.Fatal(err)
	}
	res, _ := job.Result()
	var exit *exec.ExitError
	if res.ExitCode != 7 || !errors.As(res.Err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("lost completed shell failure: code=%d err=%v output=%q", res.ExitCode, res.Err, res.Output)
	}
}
