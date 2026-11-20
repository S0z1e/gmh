//go:build !windows

package workspace

import (
	"os"
	"syscall"
)

const trustReadFlags = syscall.O_NONBLOCK

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
