package tools

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const bashShellCompletionPolicy = "once the shell exits, remaining processes in its group are stopped after a bounded output drain."

// A NOTE_EXIT observation does not reap the child. Darwin's waitid also reports
// stopped children with WEXITED, so use a process exit event instead.
func waitBashExit(cmd *exec.Cmd) (bool, error) {
	syscall.ForkLock.RLock()
	kq, err := unix.Kqueue()
	if err == nil {
		unix.CloseOnExec(kq)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return false, err
	}
	defer unix.Close(kq)
	var change unix.Kevent_t
	unix.SetKevent(&change, cmd.Process.Pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	for {
		var events [1]unix.Kevent_t
		n, err := unix.Kevent(kq, []unix.Kevent_t{change}, events[:], nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ESRCH) {
			// An already exited, unreaped child cannot be registered. We
			// still own its PID because nobody has called cmd.Wait yet.
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if n == 0 {
			continue
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			err := unix.Errno(events[0].Data)
			if errors.Is(err, unix.ESRCH) {
				return true, nil
			}
			return false, err
		}
		return true, nil
	}
}

func stopBashProcessGroup(cmd *exec.Cmd) error {
	err := killProcessTree(cmd)
	if !errors.Is(err, unix.EPERM) {
		return err
	}
	// Darwin returns EPERM when a group contains only zombies. The leader
	// remains unreaped here, so its reserved PID cannot name a new group.
	procs, queryErr := unix.SysctlKinfoProcSlice("kern.proc.pgrp", cmd.Process.Pid)
	if queryErr != nil {
		return err
	}
	for _, p := range procs {
		if p.Proc.P_stat != 5 { // SZOMB in Darwin's sys/proc.h.
			return err
		}
	}
	return os.ErrProcessDone
}
