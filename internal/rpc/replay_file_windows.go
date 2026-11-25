//go:build windows

package rpc

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	replayKernel32   = syscall.NewLazyDLL("kernel32.dll")
	replayLockFile   = replayKernel32.NewProc("LockFileEx")
	replayUnlockFile = replayKernel32.NewProc("UnlockFileEx")
)

func lockReplay(f *os.File) (func(), error) {
	overlapped := syscall.Overlapped{OffsetHigh: 0x40000000}
	ok, _, err := replayLockFile.Call(f.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok == 0 {
		return nil, err
	}
	return func() {
		_, _, _ = replayUnlockFile.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	}, nil
}

func checkReplayLinks(f *os.File) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("rpc replay log must have exactly one hard link")
	}
	return nil
}
