//go:build !darwin && !linux && !windows

package workspace

import (
	"fmt"
	"os"
)

// Untrusted/global-config operation remains available on other platforms.
// Persisting trust requires a platform-specific stable directory incarnation.
func directoryID(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root is not a directory: %s", path)
	}
	return "", nil
}

// DirectoryFileID has no portable directory incarnation on this platform.
func DirectoryFileID(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root is not a directory")
	}
	return "", nil
}
