package workspace

import (
	"fmt"
	"os"
	"syscall"
)

func directoryID(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root is not a directory: %s", path)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Birthtimespec.Sec == 0 && stat.Birthtimespec.Nsec == 0 {
		return "", nil
	}
	return fmt.Sprintf("darwin:%d:%d:%d:%d", stat.Dev, stat.Ino, stat.Birthtimespec.Sec, stat.Birthtimespec.Nsec), nil
}
