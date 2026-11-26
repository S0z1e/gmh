package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

func isWindowsProcessMissing(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

func processGroupID(pid int) int {
	if pid <= 0 {
		return 0
	}
	return pid
}

func inspectProcess(pid int) processInfo {
	if pid <= 0 {
		return processInfo{}
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processInfo{}
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != 259 { // STILL_ACTIVE
		return processInfo{}
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return processInfo{}
	}
	return processInfo{
		Alive:      true,
		PGID:       pid,
		StartToken: fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime),
	}
}

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(id.PID))
	if err != nil {
		if isWindowsProcessMissing(err) {
			return os.ErrProcessDone
		}
		return err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return err
	}
	if code != 259 {
		return os.ErrProcessDone
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return err
	}
	info := processInfo{
		Alive:      true,
		PGID:       id.PID,
		StartToken: fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime),
	}
	if !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(id.PID)).Run(); err != nil {
		return windows.TerminateProcess(h, 1)
	}
	return nil
}

func stopAdoptedProcessGroupAfterExit(proof processGroupProof) error {
	return stopVerifiedProcessGroup(proof.Leader)
}

func waitAdoptedProcess(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
	if !id.Valid() {
		return -1, false, ErrProcessIdentityMismatch
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(id.PID))
	if err != nil {
		if isWindowsProcessMissing(err) {
			return -1, false, errAdoptedProcessExited
		}
		return -1, false, err
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return -1, false, err
	}
	info := processInfo{
		Alive:      true,
		PGID:       id.PID,
		StartToken: fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime),
	}
	if !id.Matches(info) {
		return -1, false, ErrProcessIdentityMismatch
	}

	cancelEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return -1, false, err
	}
	defer windows.CloseHandle(cancelEvent)

	stopWake := context.AfterFunc(ctx, func() { _ = windows.SetEvent(cancelEvent) })
	defer stopWake()
	if ctx.Err() != nil {
		return -1, false, ctx.Err()
	}

	ev, err := windows.WaitForMultipleObjects([]windows.Handle{h, cancelEvent}, false, windows.INFINITE)
	if err != nil {
		return -1, false, err
	}
	if ev == uint32(windows.WAIT_OBJECT_0)+1 {
		return -1, false, ctx.Err()
	}
	if ev != uint32(windows.WAIT_OBJECT_0) {
		return -1, false, fmt.Errorf("unexpected process wait status %d", ev)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return -1, false, errAdoptedProcessExited
	}
	if code == 259 {
		return -1, false, errAdoptedProcessExited
	}
	if code != 0 {
		return int(code), true, fmt.Errorf("exit: exit status %d", code)
	}
	return 0, true, nil
}
