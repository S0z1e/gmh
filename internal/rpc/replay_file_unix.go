//go:build !windows

package rpc

import (
	"fmt"
	"os"
	"syscall"
)

func lockReplay(f *os.File) (func(), error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
	}
}

func checkReplayLinks(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 {
		return fmt.Errorf("rpc replay log must have exactly one hard link")
	}
	return nil
}
