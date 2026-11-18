package guidance

import (
	"fmt"
	"os"
	"path/filepath"
)

// The caller pinned this exact directory without following links. os.Root
// cannot take an existing descriptor; reopen the directory and verify it is
// still that object before letting callers read anything through the handle.
func openVerifiedScope(path string, pinned *os.File) (*os.Root, error) {
	defer pinned.Close()
	expected, err := pinned.Stat()
	if err != nil {
		return nil, err
	}
	// Force a directory lookup even if the final component has concurrently
	// become a special file. A bare os.OpenRoot can block opening a FIFO.
	root, err := os.OpenRoot(path + string(filepath.Separator) + ".")
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !actual.IsDir() || !os.SameFile(expected, actual) {
		root.Close()
		return nil, fmt.Errorf("guidance scope changed while opening: %s", path)
	}
	return root, nil
}
