package session

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

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

func writerProcessIdentity(pid int) (string, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) {
			return "", nil
		}
		return "", err
	}
	// The process name can contain spaces and parentheses. The start time
	// is field 22, or index 19 when counting from the state (field 3).
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return "", fmt.Errorf("unreadable process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("unreadable process stat")
	}
	return fields[19], nil
}

func terminateWriterProcess(owner LockOwner) error {
	fd, err := unix.PidfdOpen(owner.PID, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errWriterExited
		}
		return err
	}
	defer unix.Close(fd)
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
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errWriterExited
		}
		return err
	}
	return nil
}
