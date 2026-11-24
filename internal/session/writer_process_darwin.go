package session

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func writerProcessIdentity(pid int) string {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(proc.Proc.P_pid) != pid || proc.Proc.P_starttime.Sec <= 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", proc.Proc.P_starttime.Sec, proc.Proc.P_starttime.Usec)
}

func terminateWriterProcess(owner LockOwner) error {
	if writerProcessIdentity(owner.PID) != owner.ProcessStarted {
		return fmt.Errorf("process identity changed")
	}
	return unix.Kill(owner.PID, unix.SIGTERM)
}
