//go:build !windows

package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// All transcript appends and recovery share an OS lock. Separate descriptors
// (including descriptors in the same process) must serialize before scanning
// or writing; a process crash automatically releases the lock.
func lockTranscript(f *os.File) (func(), error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
	}
}

func tryLockWriter(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrSessionBusy
		}
		return err
	}
}

func checkTranscriptLinks(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		return fmt.Errorf("session transcript must have exactly one hard link")
	}
	return nil
}

const metadataReadFlags = syscall.O_NONBLOCK
const lockOwnerReadOffset = 0

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
