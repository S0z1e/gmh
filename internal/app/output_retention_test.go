package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/outputreceipt"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestOutputRetentionAfterShutdownAndColdResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := session.Open("expire", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	e := &tools.Executor{Root: root, OutputDir: s.OutputDir(), OutputMaxBytes: 4}
	e.SwitchSession(s.ID)
	a := &App{Root: root, Store: s, Cfg: &agent.Config{Exec: e}}
	job, err := e.BashBackground(context.Background(), tools.BashArgs{Command: "printf old!"})
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(s.OutputDir()+"-delivered", filepath.Base(job.OutputFile))
	old := time.Now().Add(-outputreceipt.Retention - time.Hour)
	if err := os.Chtimes(receipt, old, old); err != nil {
		t.Fatal(err)
	}
	s, err = session.Open("expire", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next := &tools.Executor{Root: root, OutputDir: s.OutputDir(), OutputMaxBytes: 4}
	if _, err := next.Bash(context.Background(), tools.BashArgs{Command: "printf next"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(job.OutputFile); !os.IsNotExist(err) {
		t.Fatalf("expired output retained: %v", err)
	}
	history, err := session.Load("expire")
	if err != nil || len(history) != 1 || history[0].Kind != "job" || history[0].Observation == nil {
		t.Fatalf("durable result lost: %+v, %v", history, err)
	}
}
