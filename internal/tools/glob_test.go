package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupGlobRoot(t *testing.T) *Executor {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"a.go", "b.txt", "sub/c.go", "sub/d.txt", "sub/deep/e.go"} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Executor{Root: root}
}

func TestGlobDoubleStar(t *testing.T) {
	ex := setupGlobRoot(t)
	out, _, err := ex.Glob(context.Background(), GlobArgs{Pattern: "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.go", "sub/c.go", "sub/deep/e.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "b.txt") || strings.Contains(out, "d.txt") {
		t.Fatalf("should not match .txt:\n%s", out)
	}
}

func TestGlobNoSlashMatchesAnyDepth(t *testing.T) {
	// "*.go" (no slash) matches the basename at any depth, like rg -g.
	ex := setupGlobRoot(t)
	out, _, err := ex.Glob(context.Background(), GlobArgs{Pattern: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.go", "sub/c.go", "sub/deep/e.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s:\n%s", want, out)
		}
	}
}

func TestGlobSingleDirSegment(t *testing.T) {
	ex := setupGlobRoot(t)
	out, _, err := ex.Glob(context.Background(), GlobArgs{Pattern: "sub/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sub/c.go") {
		t.Fatalf("missing sub/c.go:\n%s", out)
	}
	if strings.Contains(out, "sub/deep/e.go") {
		t.Fatalf("sub/*.go should not match sub/deep/e.go:\n%s", out)
	}
}

func TestGlobLimitMarksTruncation(t *testing.T) {
	ex := setupGlobRoot(t)
	out, _, err := ex.Glob(context.Background(), GlobArgs{Pattern: "**/*", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated: 2 files shown, 5 total]") {
		t.Fatalf("expected truncation marker, got:\n%s", out)
	}
}

func TestGlobCountIsTotal(t *testing.T) {
	ex := setupGlobRoot(t)
	// Capped to 2 shown files, the count is still the total found.
	_, n, err := ex.Glob(context.Background(), GlobArgs{Pattern: "**/*", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("count = %d, want 5", n)
	}
	_, n, err = ex.Glob(context.Background(), GlobArgs{Pattern: "**/*.rs"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no-match count = %d, want 0", n)
	}
}

func TestGlobNoMatches(t *testing.T) {
	ex := setupGlobRoot(t)
	out, _, err := ex.Glob(context.Background(), GlobArgs{Pattern: "**/*.rs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no files match") {
		t.Fatalf("expected no files match, got:\n%s", out)
	}
}

func TestGlobStaysInsideRoot(t *testing.T) {
	ex := setupGlobRoot(t)
	_, _, err := ex.Glob(context.Background(), GlobArgs{Pattern: "**/*.go", Path: ".."})
	if err == nil {
		t.Fatal("expected error for path outside root")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "sub/deep/e.go", true},
		{"*.go", "sub/c.go", true},
		{"src/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/deep/a.ts", false},
		{"**", "a/b/c", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.path); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
