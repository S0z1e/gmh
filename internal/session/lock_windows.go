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
	// Reserve a byte beyond transcript data so Windows' mandatory byte locks
	// do not block ordinary history readers. LockFileEx permits ranges beyond
	// EOF without extending the file. The handle needs GENERIC_READ (O_RDWR).
	// https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex
	// These are synchronous handles: lock acquisition waits.
	overlapped := syscall.Overlapped{OffsetHigh: 0x40000000}
	ok, _, err := sessionLockFile.Call(f.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok == 0 {
		return nil, err
	}
	return func() { _, _, _ = sessionUnlockFile.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped))) }, nil
}
