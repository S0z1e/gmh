package tools

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultGlobLimit is the default cap on reported paths.
	DefaultGlobLimit = 100
	// MaxGlobLimit is the hard cap a caller can request.
	MaxGlobLimit = 500
)

type GlobArgs struct {
	Pattern string `json:"pattern"`         // glob, e.g. "**/*.go" or "src/*.ts"
	Path    string `json:"path,omitempty"`  // search root, default "."
	Limit   int    `json:"limit,omitempty"` // default 100, max 500
}

// Glob lists files matching Pattern under Path, respecting .gitignore
// (via ripgrep when available). Results are sorted and capped;
// truncation is always reported, never silent.
func (e *Executor) Glob(ctx context.Context, a GlobArgs) (string, error) {
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("glob: empty pattern")
	}
	root := a.Path
	if root == "" {
		root = "."
	}
	dir, err := e.resolve(root)
	if err != nil {
		return "", err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = DefaultGlobLimit
	}
	if limit > MaxGlobLimit {
		limit = MaxGlobLimit
	}

	if _, err := exec.LookPath("rg"); err == nil {
		return e.globRg(ctx, dir, a.Pattern, limit)
	}
	out, err := e.globWalk(dir, a.Pattern, limit)
	return "warning: ripgrep (rg) not found, fell back to directory walk\n" + out, err
}

func (e *Executor) globRg(ctx context.Context, dir, pattern string, limit int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rg", "--files", "-g", pattern, "--", dir)
	cmd.Dir = e.Root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var paths []string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		p := strings.TrimSpace(scanner.Text())
		if p == "" {
			continue
		}
		rel, rerr := filepath.Rel(e.Root, p)
		if rerr != nil {
			rel = p
		}
		paths = append(paths, rel)
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && len(paths) == 0 {
			return "no files match " + pattern, nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("glob timed out after 30s")
		}
		if len(paths) == 0 {
			return "", fmt.Errorf("rg --files failed: %w", err)
		}
	}
	sort.Strings(paths)
	return formatGlobPaths(paths, limit, pattern), nil
}

// globWalk is the stdlib fallback: walk the tree, match each relative
// path against the pattern segment by segment, skipping hidden dirs.
func (e *Executor) globWalk(dir, pattern string, limit int) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, do not fail the walk
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || (strings.HasPrefix(name, ".") && p != dir) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(e.Root, p)
		if rerr != nil {
			return nil
		}
		if matchGlob(pattern, rel) {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "no files match " + pattern, nil
	}
	sort.Strings(paths)
	return formatGlobPaths(paths, limit, pattern), nil
}

// matchGlob matches a slash-separated path against a glob pattern where
// "**" crosses directories and "*" / "?" match within one segment.
// A pattern with no slash (e.g. "*.go") matches the basename at any depth,
// matching ripgrep's -g behaviour so both backends agree.
func matchGlob(pattern, path string) bool {
	if !strings.Contains(pattern, "/") {
		base := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base = path[i+1:]
		}
		ok, err := filepath.Match(pattern, base)
		return err == nil && ok
	}
	pSegs := strings.Split(pattern, "/")
	fSegs := strings.Split(path, "/")
	return matchSegs(pSegs, fSegs)
}

func matchSegs(p, f []string) bool {
	if len(p) == 0 {
		return len(f) == 0
	}
	if p[0] == "**" {
		// "**" matches zero or more segments.
		for i := 0; i <= len(f); i++ {
			if matchSegs(p[1:], f[i:]) {
				return true
			}
		}
		return false
	}
	if len(f) == 0 {
		return false
	}
	ok, err := filepath.Match(p[0], f[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(p[1:], f[1:])
}

func formatGlobPaths(paths []string, limit int, pattern string) string {
	if len(paths) == 0 {
		return "no files match " + pattern
	}
	total := len(paths)
	if total > limit {
		paths = paths[:limit]
	}
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p)
		b.WriteString("\n")
	}
	if total > limit {
		fmt.Fprintf(&b, "... [truncated: %d files shown, %d total]\n", limit, total)
	}
	return strings.TrimRight(b.String(), "\n")
}
