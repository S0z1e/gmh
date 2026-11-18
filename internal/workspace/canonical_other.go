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

func identify(path string) (Identity, error) {
	canonical, err := CanonicalDirectory(path)
	if err != nil {
		return Identity{}, err
	}
	id, err := directoryID(canonical)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Path: canonical, FileID: id}, nil
}
