// Package workdir defines the cwd spelling actually passed to Bash. Keep lexical
// normalization before symlink resolution consistent between execution and proof.
package workdir

import (
	"path/filepath"
	"strings"
)

// Resolve preserves the root when cwd is blank; an explicit cwd is cleaned
// lexically, including absolute symlink/.. paths, before the OS resolves it.
func Resolve(root, cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return root
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	return filepath.Clean(cwd)
}

// Canonical resolves the actual directory selected by Resolve, retaining its
// distinction between an implicit root and an explicitly cleaned cwd.
func Canonical(root, cwd string) (string, error) {
	dir, err := filepath.EvalSymlinks(Resolve(root, cwd))
	if err != nil {
		return "", err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}
