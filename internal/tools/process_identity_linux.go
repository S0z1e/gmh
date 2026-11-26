package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

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

func linuxBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func inspectProcess(pid int) processInfo {
	if pid <= 0 {
		return processInfo{}
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processInfo{}
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return processInfo{}
	}
	fields := strings.Fields(string(stat[end+1:]))
	// fields[0] is state (field 3), fields[2] is pgrp (field 5),
	// fields[19] is starttime (field 22).
	if len(fields) <= 19 {
		return processInfo{}
	}
	if fields[0] == "Z" || fields[0] == "X" || fields[0] == "x" {
		return processInfo{Zombie: true}
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid <= 0 {
		pgid = 0
	}
	token := fields[19]
	if boot := linuxBootID(); boot != "" {
		token = boot + ":" + fields[19]
	}
	return processInfo{
		Alive:      true,
		PGID:       pgid,
		StartToken: token,
	}
}

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	fd, err := unix.PidfdOpen(id.PID, 0)
	if err == nil {
		defer unix.Close(fd)
	} else if errors.Is(err, unix.ESRCH) {
		return os.ErrProcessDone
	} else if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
		// Check identity even on restricted pidfd errors before giving up.
		info := inspectProcess(id.PID)
		if !info.Alive {
			return os.ErrProcessDone
		}
		if !id.Matches(info) {
			return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
		}
		return err
	}

	info := inspectProcess(id.PID)
	if !info.Alive {
		return os.ErrProcessDone
	}
	if !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	if kerr := syscall.Kill(-id.PGID, syscall.SIGKILL); kerr == nil {
		return nil
	} else if !errors.Is(kerr, syscall.ESRCH) {
		return kerr
	}
	if fd >= 0 {
		if kerr := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); errors.Is(kerr, unix.ESRCH) {
			return os.ErrProcessDone
		} else {
			return kerr
		}
	}
	return os.ErrProcessDone
}

func stopAdoptedProcessGroupAfterExit(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	info := inspectProcess(id.PID)
	if info.Alive && !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	if kerr := syscall.Kill(-id.PGID, syscall.SIGKILL); kerr == nil {
		return nil
	} else if errors.Is(kerr, syscall.ESRCH) {
		return os.ErrProcessDone
	} else {
		return kerr
	}
}

func waitAdoptedProcess(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
	if !id.Valid() {
		return -1, false, ErrProcessIdentityMismatch
	}
	fd, err := unix.PidfdOpen(id.PID, 0)
	if err == nil {
		defer unix.Close(fd)
		info := inspectProcess(id.PID)
		if !info.Alive {
			return -1, false, errAdoptedProcessExited
		}
		if !id.Matches(info) {
			return -1, false, ErrProcessIdentityMismatch
		}
		wakeR, wakeW, pipeErr := os.Pipe()
		if pipeErr != nil {
			return -1, false, pipeErr
		}
		defer wakeR.Close()
		stopWake := context.AfterFunc(ctx, func() { _ = wakeW.Close() })
		defer func() {
			stopWake()
			_ = wakeW.Close()
		}()
		fds := []unix.PollFd{
			{Fd: int32(fd), Events: unix.POLLIN},
			{Fd: int32(wakeR.Fd()), Events: unix.POLLIN},
		}
		for {
			_, perr := unix.Poll(fds, -1)
			if errors.Is(perr, unix.EINTR) {
				continue
			}
			if perr != nil {
				return -1, false, perr
			}
			if fds[1].Revents != 0 {
				return -1, false, ctx.Err()
			}
			if fds[0].Revents != 0 {
				return -1, false, errAdoptedProcessExited
			}
		}
	}
	if errors.Is(err, unix.ESRCH) {
		info := inspectProcess(id.PID)
		if info.Alive && !id.Matches(info) {
			return -1, false, ErrProcessIdentityMismatch
		}
		return -1, false, errAdoptedProcessExited
	}
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
		return -1, false, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		info := inspectProcess(id.PID)
		if !info.Alive {
			return -1, false, errAdoptedProcessExited
		}
		if !id.Matches(info) {
			return -1, false, ErrProcessIdentityMismatch
		}
		select {
		case <-ctx.Done():
			return -1, false, ctx.Err()
		case <-ticker.C:
		}
	}
}
