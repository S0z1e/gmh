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

func TestInstructionSymlinkReplacementDuringReadIsRejected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(root, "AGENTS.md")
	put(t, filepath.Join(root, "first.md"), strings.Repeat("A", 64<<10))
	put(t, filepath.Join(root, "second.md"), strings.Repeat("B", 64<<10))
	if err := os.Symlink("first.md", path); err != nil {
		t.Fatal(err)
	}
	ctx := &scopeSwapContext{Context: context.Background(), swap: func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("second.md", path); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := Resolve(ctx, root, "new.go"); err == nil {
		t.Fatal("source symlink changed while descriptor stayed unchanged")
	}
}

func TestAncestorLimitDoesNotRequireReadableHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outer := filepath.Join(home, "outermost-scope")
	root := outer
	for i := 1; i < MaxAncestorLevels; i++ {
		root = filepath.Join(root, "nested")
	}
	put(t, filepath.Join(root, "AGENTS.md"), "workspace rules")
	put(t, filepath.Join(outer, "AGENTS.md"), "outermost rules")
	put(t, filepath.Join(home, "AGENTS.md"), "outside ancestor limit")
	if err := os.Chmod(home, 0100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(home, 0700); err != nil {
			t.Error(err)
		}
	})
	if handle, err := os.OpenRoot(home); err == nil {
		handle.Close()
		t.Skip("filesystem permits opening a searchable directory without read permission")
	} else if !os.IsPermission(err) {
		t.Fatal(err)
	}
	snapshot, err := Resolve(context.Background(), root, "new.go")
	if err != nil || len(snapshot.Files) != 2 || snapshot.Files[0].Prefix != "outermost rules" || snapshot.Files[1].Prefix != "workspace rules" {
		t.Fatalf("resolve readable scopes under traversal-only HOME: %+v, %v", snapshot, err)
	}
	source := snapshot.Files[0]
	chunk, err := Read(context.Background(), root, snapshot.Target, source.Path, source.SHA256, 0, 128)
	if err != nil || chunk.Text != "outermost rules" {
		t.Fatalf("read applicable source: %+v, %v", chunk, err)
	}
}
