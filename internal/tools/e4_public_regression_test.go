package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestE4PublicDirectoryPagination(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	for _, name := range []string{"a", "b", "c", "d", "z"} {
		if err := os.WriteFile(filepath.Join(e.Root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.Read(ReadArgs{Path: ".", Offset: 2, Limit: 2})
	if err != nil || strings.Contains(out, "\nz") || !strings.Contains(out, "b\nc\n... [truncated") {
		t.Fatalf("directory pagination lost: %q,%v", out, err)
	}
}

func TestE4PublicGlobPreservesCompleteFilenames(t *testing.T) {
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
	out, n, err := e.Glob(context.Background(), GlobArgs{Pattern: "**/*"})
	if err != nil || n != 3 || !strings.Contains(out, `line\nbreak.go`) || !strings.Contains(out, " leading.go") || !strings.Contains(out, "trailing.go ") {
		t.Fatalf("filenames changed: count=%d out=%q error=%v", n, out, err)
	}
}

func TestE4PublicGlobKeepsProducerFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake producer")
	}
	e := &Executor{Root: t.TempDir()}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rg"), []byte("#!/bin/sh\nprintf 'valid.go\\000'\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	out, _, err := e.Glob(context.Background(), GlobArgs{Pattern: "*"})
	if err == nil || !strings.Contains(out, "valid.go") {
		t.Fatalf("producer failure swallowed: %q,%v", out, err)
	}
}
