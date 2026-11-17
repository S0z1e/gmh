//go:build !windows

package guidance

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSpecialInstructionSourceCannotBlock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := syscall.Mkfifo(filepath.Join(root, "AGENTS.md"), 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := Resolve(context.Background(), root, "new.go"); err == nil {
		t.Fatal("FIFO accepted as instructions")
	}
	if time.Since(started) > time.Second {
		t.Fatal("opening special source blocked")
	}
}
