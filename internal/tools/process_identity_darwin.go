package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const darwinNoteExitStatus = 0x04000000

func processGroupID(pid int) int {
	if pid <= 0 {
		return 0
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid <= 0 {
		return pid
	}
	return pgid
}

func inspectProcess(pid int) processInfo {
	if pid <= 0 {
		return processInfo{}
	}
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(proc.Proc.P_pid) != pid {
		return processInfo{}
	}
	if proc.Proc.P_stat == 5 { // SZOMB in Darwin's sys/proc.h.
		return processInfo{Zombie: true}
	}
	if proc.Proc.P_starttime.Sec <= 0 && proc.Proc.P_starttime.Usec <= 0 {
		return processInfo{}
	}
	return processInfo{
		Alive:      true,
		PGID:       int(proc.Eproc.Pgid),
		StartToken: fmt.Sprintf("%d:%d", proc.Proc.P_starttime.Sec, proc.Proc.P_starttime.Usec),
	}
}

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	info := inspectProcess(id.PID)
	if !info.Alive {
		return os.ErrProcessDone
	}
	if !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	err := syscall.Kill(-id.PGID, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if errors.Is(err, unix.EPERM) {
		procs, queryErr := unix.SysctlKinfoProcSlice("kern.proc.pgrp", id.PGID)
		if queryErr == nil {
			allZombie := true
			for _, p := range procs {
				if p.Proc.P_stat != 5 {
					allZombie = false
					break
				}
			}
			if allZombie {
				return os.ErrProcessDone
			}
		}
	}
	return err
}

func verifyBeforeWait(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	info := inspectProcess(id.PID)
	if !info.Alive {
		return errAdoptedProcessExited
	}
	if !id.Matches(info) {
		return ErrProcessIdentityMismatch
	}
	return nil
}

func waitAdoptedProcess(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
	if err := verifyBeforeWait(id); err != nil {
		return -1, false, err
	}
	syscall.ForkLock.RLock()
	kq, err := unix.Kqueue()
	if err == nil {
		unix.CloseOnExec(kq)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, false, err
	}
	defer unix.Close(kq)

	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		return -1, false, err
	}
	defer wakeR.Close()
	stopWake := context.AfterFunc(ctx, func() { _ = wakeW.Close() })
	defer func() {
		stopWake()
		_ = wakeW.Close()
	}()

	var procChange unix.Kevent_t
	unix.SetKevent(&procChange, id.PID, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	procChange.Fflags = unix.NOTE_EXIT | darwinNoteExitStatus
	var wakeChange unix.Kevent_t
	unix.SetKevent(&wakeChange, int(wakeR.Fd()), unix.EVFILT_READ, unix.EV_ADD)

	if _, err := unix.Kevent(kq, []unix.Kevent_t{procChange, wakeChange}, nil, nil); err != nil {
		if errors.Is(err, unix.ESRCH) {
			if verr := verifyBeforeWait(id); errors.Is(verr, ErrProcessIdentityMismatch) {
				return -1, false, ErrProcessIdentityMismatch
			}
			return -1, false, errAdoptedProcessExited
		}
		return -1, false, err
	}

	if err := verifyBeforeWait(id); err != nil {
		return -1, false, err
	}

	var events [2]unix.Kevent_t
	for {
		n, err := unix.Kevent(kq, nil, events[:], nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return -1, false, err
		}
		for i := 0; i < n; i++ {
			ev := events[i]
			if ev.Filter == unix.EVFILT_READ && int(ev.Ident) == int(wakeR.Fd()) {
				return -1, false, ctx.Err()
			}
			if ev.Filter == unix.EVFILT_PROC && int(ev.Ident) == id.PID {
				if ev.Flags&unix.EV_ERROR != 0 {
					kerr := unix.Errno(ev.Data)
					if errors.Is(kerr, unix.ESRCH) {
						return -1, false, errAdoptedProcessExited
					}
					return -1, false, kerr
				}
				if ev.Fflags&darwinNoteExitStatus != 0 {
					ws := syscall.WaitStatus(ev.Data)
					if ws.Exited() {
						code := ws.ExitStatus()
						if code != 0 {
							return code, true, fmt.Errorf("exit: exit status %d", code)
						}
						return 0, true, nil
					}
					if ws.Signaled() {
						return -1, true, fmt.Errorf("exit: signal: %s", ws.Signal())
					}
				}
				return -1, false, errAdoptedProcessExited
			}
		}
	}
}
