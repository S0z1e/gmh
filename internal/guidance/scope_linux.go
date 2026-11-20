package guidance

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Walk the already canonical path without following any new symlinks.
// O_PATH needs no directory read permission on traversal-only ancestors.
func openCanonicalScope(path string) (*os.Root, error) {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, &os.PathError{Op: "open guidance scope", Path: path, Err: err}
		}
		fd = next
	}
	return openVerifiedScope(path, os.NewFile(uintptr(fd), path))
}
