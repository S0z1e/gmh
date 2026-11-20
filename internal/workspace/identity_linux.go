package workspace

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func directoryID(path string) (string, error) {
	// O_PATH pins the directory without requiring permission to enumerate it.
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	return directoryFDID(fd, path)
}

// DirectoryFileID identifies a pinned directory handle without reopening its path.
func DirectoryFileID(f *os.File) (string, error) { return directoryFDID(int(f.Fd()), f.Name()) }

func directoryFDID(fd int, path string) (string, error) {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return "", err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", fmt.Errorf("workspace root is not a directory: %s", path)
	}
	var stat unix.Statx_t
	err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_INO|unix.STATX_BTIME, &stat)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if stat.Mask&(unix.STATX_INO|unix.STATX_BTIME) != unix.STATX_INO|unix.STATX_BTIME {
		return "", nil
	}
	return fmt.Sprintf("linux:%d:%d:%d:%d:%d", stat.Dev_major, stat.Dev_minor, stat.Ino, stat.Btime.Sec, stat.Btime.Nsec), nil
}
