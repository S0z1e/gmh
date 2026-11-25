package session

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func writerProcessIdentity(pid int) (string, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return "", nil
		}
		return "", err
	}
	if int(proc.Proc.P_pid) != pid || proc.Proc.P_starttime.Sec <= 0 {
		return "", fmt.Errorf("process identity unavailable")
	}
	return fmt.Sprintf("%d:%d", proc.Proc.P_starttime.Sec, proc.Proc.P_starttime.Usec), nil
}

func writerOwnership(owner LockOwner) (alive, proven bool, err error) {
	if owner.PID <= 0 || owner.ProcessStarted == "" {
		return false, false, nil
	}
	identity, err := writerProcessIdentity(owner.PID)
	if err != nil {
		return false, false, err
	}
	if identity == "" {
		return false, true, nil
	}
	if identity != owner.ProcessStarted {
		return false, true, nil
	}
	return processAlive(owner.PID), true, nil
}

func terminateWriterProcess(owner LockOwner) error {
	identity, err := writerProcessIdentity(owner.PID)
	if err != nil {
		return err
	}
	if identity == "" {
		return errWriterExited
	}
	if identity != owner.ProcessStarted {
		return fmt.Errorf("process identity changed")
	}
	if err := unix.Kill(owner.PID, unix.SIGTERM); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errWriterExited
		}
		return err
	}
	return nil
}
