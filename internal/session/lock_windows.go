package session

import (
	"fmt"
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

func tryLockWriter(f *os.File) error {
	overlapped := syscall.Overlapped{}
	ok, _, err := sessionLockFile.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok == 0 {
		if err == syscall.Errno(33) {
			return ErrSessionBusy
		} // ERROR_LOCK_VIOLATION
		return err
	}
	return nil
}

func checkTranscriptLinks(f *os.File) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("session transcript must have exactly one hard link")
	}
	return nil
}

const metadataReadFlags = 0
const lockOwnerReadOffset = 1

func processAlive(pid int) bool {
	return pid > 0
}
