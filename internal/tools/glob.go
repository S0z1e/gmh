package tools

import (
	"context"
	"fmt"
	"io"
	"minimalharness/internal/fileguard"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const DefaultGlobLimit = 100
const MaxGlobLimit = 500

type GlobArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (e *Executor) Glob(ctx context.Context, a GlobArgs) (string, int, error) {
	r, err := e.globResult(ctx, a)
	return r.Text, r.Count, err
}

func (e *Executor) globResult(ctx context.Context, a GlobArgs) (ToolResult, error) {
	if strings.TrimSpace(a.Pattern) == "" || len(a.Pattern) > 4096 || len(strings.Split(a.Pattern, "/")) > 128 {
		return ToolResult{}, &InputError{Err: fmt.Errorf("glob: invalid or excessive pattern")}
	}

	root := a.Path
	if root == "" {
		root = "."
	}
	dir, err := e.resolve(root)
	if err != nil {
		return ToolResult{}, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = DefaultGlobLimit
	}
	limit = min(limit, MaxGlobLimit)
	if _, err := exec.LookPath("rg"); err == nil {
		return e.globRgResult(ctx, dir, a.Pattern, limit)
	}
	r, err := e.globWalkResult(ctx, dir, a.Pattern, limit)
	r.Text = "warning: ripgrep (rg) not found, fell back to directory walk\n" + r.Text
	return r, err
}

func (e *Executor) globRg(ctx context.Context, dir, pattern string, limit int) (string, int, error) {
	r, err := e.globRgResult(ctx, dir, pattern, limit)
	return r.Text, r.Count, err
}

func (e *Executor) globRgResult(ctx context.Context, dir, pattern string, limit int) (ToolResult, error) {
	rel, err := filepath.Rel(e.Root, dir)
	if err != nil {
		return ToolResult{}, err
	}
	paths := pathWindow{limit: limit}
	truncated, err := e.streamSearch(ctx, "rg", []string{"--files", "-0", "-g", pattern, "--", rel}, 0, "glob", func(path string) error {
		if path == "" {
			return fmt.Errorf("glob: empty path record")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.Root, path)
		}
		rel, err := fileguard.RelativePath(e.Root, path)
		if err != nil {
			return err
		}
		paths.add(filepath.ToSlash(rel))
		return nil
	})
	return globPathsResult(paths, pattern, truncated, err != nil), err
}

func (e *Executor) globWalk(dir, pattern string, limit int) (string, int, error) {
	r, err := e.globWalkResult(context.Background(), dir, pattern, limit)
	return r.Text, r.Count, err
}

func (e *Executor) globWalkResult(ctx context.Context, dir, pattern string, limit int) (ToolResult, error) {
	for _, segment := range strings.Split(pattern, "/") {
		if _, err := filepath.Match(segment, ""); err != nil {
			return ToolResult{}, &InputError{Err: fmt.Errorf("glob: %w", err)}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	root, rel, err := fileguard.Root(e.Root, dir)
	if err != nil {
		return ToolResult{}, err
	}
	defer root.Close()
	paths := pathWindow{limit: limit}
	scanned := 0
	truncated := false
	var walk func(string, int) error
	walk = func(path string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 128 {
			truncated = true
			return fmt.Errorf("glob: directory depth limit reached")
		}
		f, err := root.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := f.ReadDir(256)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				scanned++
				if scanned > maxDirectoryEntries {
					truncated = true
					return nil
				}
				name := entry.Name()
				child := filepath.Join(path, name)
				if entry.IsDir() {
					if name == ".git" || name == "node_modules" || strings.HasPrefix(name, ".") {
						continue
					}
					if err := walk(child, depth+1); err != nil {
						return err
					}
					if truncated {
						return nil
					}
				} else if matchGlob(pattern, filepath.ToSlash(child)) {
					paths.add(filepath.ToSlash(child))
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	err = walk(rel, 0)
	return globPathsResult(paths, pattern, truncated, err != nil), err
}

func globPathsResult(paths pathWindow, pattern string, truncated, failed bool) ToolResult {
	incomplete := truncated || failed
	var b strings.Builder
	for _, path := range paths.sorted() {
		b.WriteString(displayPath(path))
		b.WriteByte('\n')
	}
	if paths.total == 0 && !incomplete {
		fmt.Fprintf(&b, "no files match %s", pattern)
	}
	if paths.total > paths.limit {
		fmt.Fprintf(&b, "... [truncated: %d files shown, %d total]\n", paths.limit, paths.total)
	}
	if incomplete {
		b.WriteString("... [search incomplete; count is a lower bound; narrow the path or pattern]\n")
	}
	return ToolResult{Text: strings.TrimSuffix(b.String(), "\n"), Count: paths.total, Truncated: incomplete || paths.total > paths.limit, CountLowerBound: incomplete}
}
