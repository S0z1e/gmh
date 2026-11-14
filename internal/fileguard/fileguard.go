// Package fileguard confines file operations to a directory using os.Root.
package fileguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root returns an open root and a relative name. The caller closes the root.
func Root(base, path string) (*os.Root, string, error) {
	if path == "" {
		return nil, "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, "", err
	}
	full := filepath.Clean(path)
	if !filepath.IsAbs(full) {
		full = filepath.Join(abs, full)
	}
	rel, err := filepath.Rel(abs, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("path outside workspace root: %s", path)
	}
	r, err := os.OpenRoot(abs)
	return r, rel, err
}

func Open(base, path string) (*os.File, error) {
	r, rel, err := Root(base, path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Open(rel)
}

func ReadFile(base, path string) ([]byte, error) {
	r, rel, err := Root(base, path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.ReadFile(rel)
}

// Validate also checks symlink traversal for paths passed to search processes.
// Missing leaf names are allowed so a caller can create a file safely with Root.
func Validate(base, path string) (string, error) {
	r, rel, err := Root(base, path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	for name := rel; ; name = filepath.Dir(name) {
		_, err := r.Stat(name)
		if err == nil {
			return filepath.Join(base, rel), nil
		}
		if !os.IsNotExist(err) || name == "." {
			return "", err
		}
	}
}
