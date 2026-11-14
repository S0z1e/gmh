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
