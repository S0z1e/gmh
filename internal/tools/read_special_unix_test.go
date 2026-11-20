//go:build !windows

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadFIFORejectsWithoutOpeningWait(t *testing.T) {
	if dir := os.Getenv("GMH_READ_FIFO_PROBE"); dir != "" {
		e := &Executor{Root: dir}
		if _, err := e.Read(ReadArgs{Path: "fifo"}); err == nil {
			t.Fatal("FIFO accepted")
		}
		return
	}
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadFIFORejectsWithoutOpeningWait$")
	cmd.Env = append(os.Environ(), "GMH_READ_FIFO_PROBE="+dir, "GORACE=atexit_sleep_ms=0")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err != nil {
		t.Fatalf("FIFO read blocked/failed: %v,%v,%s", ctx.Err(), err, out)
	}
}
