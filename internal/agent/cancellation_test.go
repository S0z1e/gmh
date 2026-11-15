package agent

import (
	"context"
	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestForegroundBashCancellation(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{Exec: &tools.Executor{Root: root, BackgroundThreshold: 5 * time.Second}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	cfg.execBash(ctx, llm.ToolCall{Function: llm.FunctionCall{Name: "bash", Arguments: `{"command":"sleep 0.5; echo bad > survived"}`}})
	if time.Since(start) > 350*time.Millisecond {
		t.Error("cancel did not interrupt foreground wait")
	}
	time.Sleep(550 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(root, "survived")); err == nil {
		t.Error("cancelled command kept running")
	}
}

func TestCancelledTurnPreservesSessionBackgroundJob(t *testing.T) {
	e := &tools.Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	job, err := e.BashBackground(context.Background(), tools.BashArgs{Command: "sleep 0.2; echo survived", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.CancelJobs()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := &Config{Exec: e, Client: &llm.Client{APIKey: "fake", Model: "test"}}
	_, _, _ = cfg.Run(ctx, []llm.Message{{Role: "user", Content: "cancelled task"}})
	select {
	case <-job.Done():
		if result, _ := job.Result(); result.Err != nil {
			t.Fatalf("background job cancelled: %v", result.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("background job did not finish")
	}
}
