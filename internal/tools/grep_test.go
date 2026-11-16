package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestGrepOutputCapReturnsCollectedMatches(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep unavailable")
	}
	e := &Executor{Root: t.TempDir()}
	if err := os.WriteFile(filepath.Join(e.Root, "many.txt"), []byte(strings.Repeat("MATCH payload\n", 150000)), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, count, err := e.Grep(ctx, GrepArgs{Pattern: "MATCH", MaxMatches: 10})
	if err != nil {
		t.Fatalf("output cap lost matches: %v", err)
	}
	if count < 10 || !strings.Contains(out, "MATCH payload") || !strings.Contains(out, "at least") || !strings.Contains(out, "output limit") {
		t.Fatalf("incomplete count not marked: count=%d output=%s", count, out)
	}
}

func TestGrepLongLineIsBounded(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	if err := os.WriteFile(filepath.Join(e.Root, "long.txt"), []byte("MATCH\n"+strings.Repeat("MATCH", MaxGrepBytes)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, count, err := e.Grep(ctx, GrepArgs{Pattern: "MATCH"})
	if err != nil || count < 1 || !strings.Contains(out, "long.txt:1:") || !strings.Contains(out, "output limit") || len(out) > MaxGrepBytes+1024 {
		t.Fatalf("unbounded or lost long-line result: bytes=%d count=%d err=%v", len(out), count, err)
	}
}

func TestGrepCanceledContextIsPreserved(t *testing.T) {
	e := setupGrepRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := e.Grep(ctx, GrepArgs{Pattern: "TODO"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestGrepFallbackIsBounded(t *testing.T) {
	if _, err := exec.LookPath("grep"); err != nil {
		t.Skip("grep unavailable")
	}
	for _, longLine := range []bool{false, true} {
		t.Run(fmt.Sprint(longLine), func(t *testing.T) {
			e := &Executor{Root: t.TempDir()}
			content := strings.Repeat("MATCH payload\n", 150000)
			if longLine {
				content = "MATCH\n" + strings.Repeat("MATCH", MaxGrepBytes) + "\n"
			}
			if err := os.WriteFile(filepath.Join(e.Root, "many.txt"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r, err := e.grepFallback(ctx, e.Root, GrepArgs{Pattern: "MATCH"}, 10, 0)
			if err != nil || r.Count < 1 || !r.Truncated || !r.CountLowerBound || !strings.Contains(r.Text, "many.txt:1:") || strings.Contains(r.Text, " total]") || len(r.Text) > MaxGrepBytes+1024 {
				t.Fatalf("fallback result: count=%d truncated=%v lower=%v bytes=%d err=%v", r.Count, r.Truncated, r.CountLowerBound, len(r.Text), err)
			}
		})
	}
}

func TestGrepDisplayCapKeepsExactCount(t *testing.T) {
	e := setupGrepRoot(t)
	r, err := e.Dispatch(context.Background(), "grep", `{"pattern":"TODO","maxMatches":1}`)
	if err != nil || r.Count != 3 || !r.Truncated || r.CountLowerBound {
		t.Fatalf("exact capped result: %+v %v", r, err)
	}
}

func TestGrepFallbackSkipsBinaryFiles(t *testing.T) {
	e := setupGrepRoot(t)
	if err := os.WriteFile(filepath.Join(e.Root, "binary.dat"), []byte("\x00TODO\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := e.grepFallback(context.Background(), e.Root, GrepArgs{Pattern: "TODO"}, 10, 0)
	if err != nil || r.Count != 3 || r.CountLowerBound || strings.Contains(r.Text, "binary.dat") {
		t.Fatalf("binary file broke text search: %+v %v", r, err)
	}
}

func TestGrepFindsMatchesAcrossFiles(t *testing.T) {
	ex := setupGrepRoot(t)
	out, _, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:3:") || !strings.Contains(out, "b.txt:2:") || !strings.Contains(out, "sub/c.go:2:") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGrepIncludeFiltersByGlob(t *testing.T) {
	ex := setupGrepRoot(t)
	out, _, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO", Include: "*.go"})
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
	out, _, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO", MaxMatches: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated: 1 matches shown, 3 total]") {
		t.Fatalf("expected truncation marker, got:\n%s", out)
	}
}

func TestGrepCountIsTotal(t *testing.T) {
	ex := setupGrepRoot(t)
	// Capped to 1 shown match, the count is still the total found.
	_, n, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO", MaxMatches: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
	_, n, err = ex.Grep(context.Background(), GrepArgs{Pattern: "zzz-no-such-string"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no-match count = %d, want 0", n)
	}
}

func TestGrepNoMatches(t *testing.T) {
	ex := setupGrepRoot(t)
	out, _, err := ex.Grep(context.Background(), GrepArgs{Pattern: "zzz-no-such-string"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no matches") {
		t.Fatalf("expected no matches, got:\n%s", out)
	}
}

func TestGrepStaysInsideRoot(t *testing.T) {
	ex := setupGrepRoot(t)
	_, _, err := ex.Grep(context.Background(), GrepArgs{Pattern: "TODO", Path: ".."})
	if err == nil {
		t.Fatal("expected error for path outside root")
	}
}

func TestParseGrepFallbackLine(t *testing.T) {
	path, lineNo, text, isCtx, ok := parseGrepFallbackLine("a.go:12:func main() {}")
	if !ok || isCtx || path != "a.go" || lineNo != 12 || text != "func main() {}" {
		t.Fatalf("match parse = (%q, %d, %q, %v, %v)", path, lineNo, text, isCtx, ok)
	}
	path, lineNo, text, isCtx, ok = parseGrepFallbackLine("sub/b-file.go-11-package sub")
	if !ok || !isCtx || path != "sub/b-file.go" || lineNo != 11 || text != "package sub" {
		t.Fatalf("context parse = (%q, %d, %q, %v, %v)", path, lineNo, text, isCtx, ok)
	}
}
