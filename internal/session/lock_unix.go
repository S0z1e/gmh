//go:build !windows

package session

import (
	"errors"
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
