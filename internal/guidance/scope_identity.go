package guidance

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	rootPath := path
	if runtime.GOOS != "windows" {
		// Force a directory lookup in case the leaf became a FIFO. Windows
		// has no FIFO at a filesystem path and extended paths do not normalize \.
		rootPath += string(filepath.Separator) + "."
	}
	root, err := os.OpenRoot(rootPath)
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
