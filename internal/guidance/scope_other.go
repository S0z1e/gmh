//go:build !linux && !darwin

package guidance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Step through the canonical path using confined directory handles. Check the
// observed directory ID before advancing, so an inserted link cannot redirect
// a subsequent lookup.
func openCanonicalScope(path string) (*os.Root, error) {
	volumeRoot := filepath.VolumeName(path) + string(filepath.Separator)
	parts, err := filepath.Rel(volumeRoot, path)
	if err != nil {
		return nil, err
	}
	handle, err := os.OpenRoot(volumeRoot)
	if err != nil || parts == "." {
		return handle, err
	}
	for _, part := range strings.Split(filepath.ToSlash(parts), "/") {
		observed, err := handle.Lstat(part)
		if err != nil || !observed.IsDir() {
			handle.Close()
			return nil, fmt.Errorf("guidance scope is not a canonical directory: %s", path)
		}
		next, err := handle.OpenRoot(part)
		handle.Close()
		if err != nil {
			return nil, err
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(observed, actual) {
			next.Close()
			return nil, fmt.Errorf("guidance scope changed while opening: %s", path)
		}
		handle = next
	}
	return handle, nil
}
