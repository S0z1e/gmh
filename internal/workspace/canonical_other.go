//go:build !windows

package workspace

import "path/filepath"

// CanonicalDirectory resolves the absolute path and its directory links.
func CanonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func workspaceBindingPath(path string) (string, error) {
	return CanonicalDirectory(path)
}
