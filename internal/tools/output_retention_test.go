package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"minimalharness/internal/outputreceipt"
)

func oldReceipt(t *testing.T, dir, path string) {
	t.Helper()
	if err := outputreceipt.Record(dir, path); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-outputreceipt.Retention - time.Hour)
	receipt := filepath.Join(filepath.Dir(dir), "bash-output-delivered", filepath.Base(path))
	if err := os.Chtimes(receipt, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestOutputRetentionExpiresOnlyDeliveredAndReclaimsQuota(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "bash-output")
	e := &Executor{Root: t.TempDir(), OutputDir: dir, OutputMaxBytes: 8}
	r, err := e.bashResult(ctx, BashArgs{Command: "printf 1234"})
	if err != nil {
		t.Fatal(err)
	}
	undelivered, err := e.bashResult(ctx, BashArgs{Command: "printf keep"})
	if err != nil {
		t.Fatal(err)
	}
	oldReceipt(t, dir, r.OutputFile)
	for _, fresh := range []bool{false, true} {
		if fresh {
			e = &Executor{Root: e.Root, OutputDir: dir, OutputMaxBytes: 8}
		}
		newFile, err := e.bashResult(ctx, BashArgs{Command: "printf next"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(undelivered.OutputFile); err != nil {
			t.Fatalf("undelivered output deleted: %v", err)
		}
		if _, err := os.Stat(r.OutputFile); !os.IsNotExist(err) {
			t.Fatalf("expired output retained: %v", err)
		}
		oldReceipt(t, dir, newFile.OutputFile)
	}
}

func TestOutputRetentionProtectsActiveAndRecentFiles(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "bash-output")
	e := &Executor{Root: t.TempDir(), OutputDir: dir, OutputMaxBytes: 3}
	active, err := e.newBashOutput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if _, err := active.Write([]byte("123")); err != nil {
		t.Fatal(err)
	}
	oldReceipt(t, dir, active.path)
	if _, err := e.newBashOutput(ctx); err == nil {
		t.Fatal("expired receipt freed active capture")
	}
	if _, err := os.Stat(active.path); err != nil {
		t.Fatal(err)
	}
	active.Close()
	deletePath := active.path
	newFile, err := e.newBashOutput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(deletePath); !os.IsNotExist(err) {
		t.Fatal("finished expired file retained")
	}
	if _, err := newFile.Write([]byte("123")); err != nil {
		t.Fatal(err)
	}
	newFile.Close()
	if err := outputreceipt.Record(dir, newFile.path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.newBashOutput(ctx); err == nil {
		t.Fatal("unexpired output deleted to make room")
	}
}
