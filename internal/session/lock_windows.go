package session

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	sessionKernel32   = syscall.NewLazyDLL("kernel32.dll")
	sessionLockFile   = sessionKernel32.NewProc("LockFileEx")
	sessionUnlockFile = sessionKernel32.NewProc("UnlockFileEx")
)

func lockTranscript(f *os.File) (func(), error) {
	// A fixed byte range serializes recovery with append regardless of the
	// current file size. These are synchronous handles: lock acquisition waits.
	var overlapped syscall.Overlapped
	ok, _, err := sessionLockFile.Call(f.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok == 0 {
		return nil, err
	}
	return func() { _, _, _ = sessionUnlockFile.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped))) }, nil
}
