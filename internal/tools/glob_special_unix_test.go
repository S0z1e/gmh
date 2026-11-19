//go:build !windows

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGlobFallbackFIFOIsNonblocking(t *testing.T) {
	if root := os.Getenv("GMH_GLOB_FIFO_PROBE"); root != "" {
		e := &Executor{Root: root}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		out, n, err := e.Glob(ctx, GlobArgs{Path: "fifo", Pattern: "*"})
		if err == nil {
			t.Fatalf("FIFO accepted: n=%d out=%q", n, out)
		}
		return
	}
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGlobFallbackFIFOIsNonblocking$", "-test.v")
	cmd.Env = append(os.Environ(), "GMH_GLOB_FIFO_PROBE="+root, "PATH="+t.TempDir(), "GORACE=atexit_sleep_ms=0")
	out, err := cmd.CombinedOutput()
	t.Logf("child result: %s", out)
	if ctx.Err() != nil || err != nil {
		t.Fatalf("glob blocked/failed beyond its 100ms context: %v, %v", ctx.Err(), err)
	}
}

func TestGlobFallbackAcceptsFileRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.go"), []byte("package p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Root: root}
	t.Setenv("PATH", t.TempDir())
	out, n, err := e.Glob(context.Background(), GlobArgs{Path: "file.go", Pattern: "*.go"})
	if err != nil || n != 1 || !strings.Contains(out, "file.go") {
		t.Fatalf("file search root: out=%q n=%d err=%v", out, n, err)
	}
}

func TestGlobFallbackQuotedNamesStayDistinct(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{" leading.go", `" leading.go"`} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	e := &Executor{Root: root}
	t.Setenv("PATH", t.TempDir())
	out, n, err := e.Glob(context.Background(), GlobArgs{Pattern: "*"})
	if err != nil || n != 2 {
		t.Fatalf("glob: out=%q n=%d err=%v", out, n, err)
	}
	lines := strings.Split(strings.TrimPrefix(out, "warning: ripgrep (rg) not found, fell back to directory walk\n"), "\n")
	if len(lines) != 2 || lines[0] == lines[1] {
		t.Fatalf("distinct filenames displayed identically: %q", out)
	}
}
