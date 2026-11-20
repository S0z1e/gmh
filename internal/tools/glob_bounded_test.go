package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGlobPreservesWhitespaceAndNewlineFilenames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX filename fixture")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip(err)
	}
	e := &Executor{Root: t.TempDir()}
	for _, name := range []string{" leading.go", "trailing.go ", "line\nbreak.go"} {
		if err := os.WriteFile(filepath.Join(e.Root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := e.globResult(context.Background(), GlobArgs{Pattern: "**/*"})
	if err != nil || r.Count != 3 || !strings.Contains(r.Text, `line\nbreak.go`) || !strings.Contains(r.Text, " leading.go") || !strings.Contains(r.Text, "trailing.go ") {
		t.Fatalf("paths: %+v,%v", r, err)
	}
}

func TestGlobProducerFailureKeepsPartialPathsAndError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake producer")
	}
	e := &Executor{Root: t.TempDir()}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rg"), []byte("#!/bin/sh\nprintf 'valid.go\\000'\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	r, err := e.globResult(context.Background(), GlobArgs{Pattern: "*"})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !r.CountLowerBound || !r.Truncated || r.Count != 1 || !strings.Contains(r.Text, "valid.go") {
		t.Fatalf("partial error: %+v,%v", r, err)
	}
}

func TestGlobFallbackHonorsCancellationAndKeepsSortedSubset(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	for _, name := range []string{"z.go", "c.go", "a.go", "b.go", "d.go"} {
		if err := os.WriteFile(filepath.Join(e.Root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := e.globWalkResult(context.Background(), e.Root, "*.go", 2)
	if err != nil || r.Count != 5 || !r.Truncated || r.CountLowerBound || !strings.HasPrefix(r.Text, "a.go\nb.go\n") {
		t.Fatalf("fallback page: %+v,%v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err = e.globWalkResult(ctx, e.Root, "*", 2)
	if !errors.Is(err, context.Canceled) || !r.CountLowerBound {
		t.Fatalf("cancel ignored: %+v,%v", r, err)
	}
}

func TestGlobOutputCapKeepsLowerBoundAndReapsProducer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake producer")
	}
	e := &Executor{Root: t.TempDir()}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rg"), []byte("#!/bin/sh\nwhile :; do printf 'path-%0600d.go\\000' 1; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	r, err := e.globResult(context.Background(), GlobArgs{Pattern: "*", Limit: 3})
	if err != nil || !r.CountLowerBound || !r.Truncated || r.Count < 3 || strings.Count(r.Text, "path-") != 3 {
		t.Fatalf("bounded stream: %+v,%v", r, err)
	}
}

func TestGlobPathWindowIsBoundedAndSorted(t *testing.T) {
	w := pathWindow{limit: 2}
	for _, path := range []string{"z", "c", "a", "b", "d"} {
		w.add(path)
	}
	if len(w.paths) != 2 || w.total != 5 || strings.Join(w.sorted(), ",") != "a,b" {
		t.Fatalf("window: %+v", w)
	}
}
