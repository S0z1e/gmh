//go:build !windows

package guidance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestSameSizeRewriteWithRestoredMtimeIsRejected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(root, "AGENTS.md")
	put(t, path, strings.Repeat("A", 64<<10))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &scopeSwapContext{Context: context.Background(), swap: func() {
		put(t, path, strings.Repeat("B", 64<<10))
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := Resolve(ctx, root, "new.go"); err == nil {
		t.Fatal("accepted mixed source bytes while mtime/size were restored")
	}
}
