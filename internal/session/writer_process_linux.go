package session

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func writerProcessIdentity(pid int) string {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The process name can contain spaces and parentheses. The start time
	// is field 22, or index 19 when counting from the state (field 3).
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

func terminateWriterProcess(owner LockOwner) error {
	fd, err := unix.PidfdOpen(owner.PID, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if writerProcessIdentity(owner.PID) != owner.ProcessStarted {
		return fmt.Errorf("process identity changed")
	}
	return unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
}
