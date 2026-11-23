// Package fileguard confines file operations to a directory using os.Root.
package fileguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RelativePath applies the lexical path rules shared by workspace tools.
// It does not inspect the filesystem or follow symbolic links.
func RelativePath(base, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	full := filepath.Clean(path)
	if !filepath.IsAbs(full) {
		full = filepath.Join(abs, full)
	}
	rel, err := filepath.Rel(abs, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path outside workspace root: %s", path)
	}
	return rel, nil
}

// Root returns an open root and a relative name. The caller closes the root.
func Root(base, path string) (*os.Root, string, error) {
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, "", err
	}
	rel, err := RelativePath(abs, path)
	if err != nil {
		return nil, "", err
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
	f, err := r.OpenFile(rel, os.O_RDONLY|nonblockingRead, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		f.Close()
		return nil, fmt.Errorf("read requires a regular file or directory")
	}
	return f, nil
}

// OpenRegular opens an explicitly chosen regular file, following symlinks.
// Use Open instead for paths that must stay inside a workspace.
func OpenRegular(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment requires a regular file: %q", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|nonblockingRead, 0)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("attachment requires a regular file: %q", path)
	}
	return f, nil
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
