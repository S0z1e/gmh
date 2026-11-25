//go:build windows

package session

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	windowsProcessQueryLimited     = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
	windowsProcessTerminate        = 0x0001 // PROCESS_TERMINATE
	windowsProcessSynchronize      = 0x00100000
	windowsErrorInsufficientBuffer = 122
	writerIdentityPrefix           = "win:"
)

var (
	procOpenProcess                = sessionKernel32.NewProc("OpenProcess")
	procGetProcessTimes            = sessionKernel32.NewProc("GetProcessTimes")
	procTerminateProcess           = sessionKernel32.NewProc("TerminateProcess")
	procQueryFullProcessImageNameW = sessionKernel32.NewProc("QueryFullProcessImageNameW")
)

func writerProcessIdentity(pid int) (string, error) {
	h, err := openWriterProcess(pid, windowsProcessQueryLimited)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.Errno(87) {
			return "", nil
		}
		return "", err
	}
	defer syscall.CloseHandle(h)
	return identityFromHandle(h)
}

func identityFromHandle(h syscall.Handle) (string, error) {
	created, err := processCreationFiletime(h)
	if err != nil {
		return "", err
	}
	if created == 0 {
		return "", fmt.Errorf("process creation time is unset")
	}
	image, err := processImagePath(h)
	if err != nil {
		return "", err
	}
	if image == "" {
		return "", fmt.Errorf("process image path is empty")
	}
	return fmt.Sprintf("%s%d:%s", writerIdentityPrefix, created, image), nil
}

// writerOwnership reports whether the recorded process is still the owner.
// proven is false when the PID cannot be queried. A PID that does not exist
// (OpenProcess ERROR_INVALID_PARAMETER) is a dead owner and may be taken.
// A query that fails for any other reason refuses the steal. Identity and
// liveness use the same handle so the process cannot be replaced between them.
func writerOwnership(owner LockOwner) (alive, proven bool, err error) {
	if owner.PID <= 0 || owner.ProcessStarted == "" {
		return false, false, nil
	}
	h, err := openWriterProcess(owner.PID, windowsProcessQueryLimited|windowsProcessSynchronize)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.Errno(87) {
			return false, true, nil
		}
		return false, false, err
	}
	defer syscall.CloseHandle(h)
	identity, err := identityFromHandle(h)
	if err != nil {
		return false, false, err
	}
	if identity != owner.ProcessStarted {
		return false, true, nil
	}
	exited, err := writerExited(h)
	if err != nil {
		return false, false, err
	}
	return !exited, true, nil
}

func terminateWriterProcess(owner LockOwner) error {
	h, err := openWriterProcess(owner.PID, windowsProcessQueryLimited|windowsProcessTerminate|windowsProcessSynchronize)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.Errno(87) {
			return errWriterExited
		}
		return err
	}
	defer syscall.CloseHandle(h)
	identity, err := identityFromHandle(h)
	if err != nil {
		if exited, waitErr := writerExited(h); waitErr == nil && exited {
			return errWriterExited
		}
		return err
	}
	if identity != owner.ProcessStarted {
		return fmt.Errorf("process identity changed")
	}
	exited, err := writerExited(h)
	if err != nil {
		return err
	}
	if exited {
		return errWriterExited
	}
	ok, _, callErr := procTerminateProcess.Call(uintptr(h), 1)
	if ok == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == syscall.Errno(87) {
			return errWriterExited
		}
		if errno, ok := callErr.(syscall.Errno); ok && errno == syscall.Errno(5) {
			if exited, waitErr := writerExited(h); waitErr == nil && exited {
				return errWriterExited
			}
		}
		return callErr
	}
	return nil
}

func writerExited(h syscall.Handle) (bool, error) {
	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return false, err
	}
	return event == syscall.WAIT_OBJECT_0, nil
}

func openWriterProcess(pid, access int) (syscall.Handle, error) {
	if pid <= 0 {
		return 0, syscall.EINVAL
	}
	h, _, err := procOpenProcess.Call(uintptr(access), 0, uintptr(pid))
	if h == 0 {
		if err == nil || err == syscall.Errno(0) {
			return 0, syscall.EINVAL
		}
		return 0, err
	}
	return syscall.Handle(h), nil
}

func processCreationFiletime(h syscall.Handle) (uint64, error) {
	var created, exited, kernel, user syscall.Filetime
	ok, _, err := procGetProcessTimes.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&created)),
		uintptr(unsafe.Pointer(&exited)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ok == 0 {
		return 0, err
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}

func processImagePath(h syscall.Handle) (string, error) {
	buf := make([]uint16, 260)
	for {
		size := uint32(len(buf))
		ok, _, err := procQueryFullProcessImageNameW.Call(
			uintptr(h), 0,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)),
		)
		if ok != 0 {
			if size > uint32(len(buf)) {
				return "", syscall.EINVAL
			}
			return syscall.UTF16ToString(buf[:size]), nil
		}
		if errno, isErrno := err.(syscall.Errno); !isErrno || errno != syscall.Errno(windowsErrorInsufficientBuffer) || len(buf) >= 32*1024 {
			return "", err
		}
		buf = make([]uint16, len(buf)*2)
	}
}
