package guidance

import (
	"os"

	"golang.org/x/sys/unix"
)

func openCanonicalScope(path string) (*os.Root, error) {
	// Unlike O_NOFOLLOW, O_NOFOLLOW_ANY applies to every path component.
	fd, err := unix.Open(path, unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open guidance scope", Path: path, Err: err}
	}
	return openVerifiedScope(path, os.NewFile(uintptr(fd), path))
}
