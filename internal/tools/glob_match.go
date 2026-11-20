package tools

import (
	"path/filepath"
	"strings"
)

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

// Dynamic matching avoids exponential backtracking through repeated **.
func matchSegs(p, f []string) bool {
	prev := make([]bool, len(f)+1)
	prev[0] = true
	for _, segment := range p {
		next := make([]bool, len(f)+1)
		if segment == "**" {
			next[0] = prev[0]
			for j := 1; j <= len(f); j++ {
				next[j] = prev[j] || next[j-1]
			}
		} else {
			for j := 1; j <= len(f); j++ {
				matched, err := filepath.Match(segment, f[j-1])
				next[j] = prev[j-1] && err == nil && matched
			}
		}
		prev = next
	}
	return prev[len(f)]
}
