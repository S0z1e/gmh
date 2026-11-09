package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupGrepRoot(t *testing.T) *Executor {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\n\n// TODO: fix this\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("hello\nTODO: another\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "c.go"), []byte("package sub\n// TODO: third\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Executor{Root: root}
}

func TestGrepFindsMatchesAcrossFiles(t *testing.T) {
	ex := setupGrepRoot(t)
	out, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:3:") || !strings.Contains(out, "b.txt:2:") || !strings.Contains(out, "sub/c.go:2:") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGrepIncludeFiltersByGlob(t *testing.T) {
	ex := setupGrepRoot(t)
	out, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO", Include: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "b.txt") {
		t.Fatalf("include *.go should exclude b.txt:\n%s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "sub/c.go") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGrepCapMarksTruncation(t *testing.T) {
	ex := setupGrepRoot(t)
	out, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO", MaxMatches: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated: 1 matches shown, 3 total]") {
		t.Fatalf("expected truncation marker, got:\n%s", out)
	}
}

func TestGrepNoMatches(t *testing.T) {
	ex := setupGrepRoot(t)
	out, err := ex.Grep(context.Background(), GrepArgs{Pattern: "zzz-no-such-string"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no matches") {
		t.Fatalf("expected no matches, got:\n%s", out)
	}
}

func TestGrepStaysInsideRoot(t *testing.T) {
	ex := setupGrepRoot(t)
	_, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO", Path: ".."})
	if err == nil {
		t.Fatal("expected error for path outside root")
	}
}
