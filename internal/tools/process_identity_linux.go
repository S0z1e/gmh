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
	info, _ := inspectProcessErr(pid)
	return info
}

type linuxProcMeta struct {
	info       processInfo
	ppid       int
	startTicks uint64
}

func inspectLinuxProcMeta(pid int) (linuxProcMeta, error) {
	if pid <= 0 {
		return linuxProcMeta{}, nil
	}
	procDir := fmt.Sprintf("/proc/%d", pid)
	stat, err := os.ReadFile(procDir + "/stat")
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return linuxProcMeta{}, nil
		}
		return linuxProcMeta{}, err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return linuxProcMeta{}, nil
	}
	fields := strings.Fields(string(stat[end+1:]))
	// fields[0] is state (field 3), fields[1] is ppid (field 4),
	// fields[2] is pgrp (field 5), fields[3] is session (field 6),
	// fields[19] is starttime (field 22).
	if len(fields) <= 19 {
		return linuxProcMeta{}, nil
	}
	if fields[0] == "Z" || fields[0] == "X" || fields[0] == "x" {
		return linuxProcMeta{info: processInfo{Zombie: true}}, nil
	}
	ppid, _ := strconv.Atoi(fields[1])
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid <= 0 {
		pgid = 0
	}
	sid, _ := strconv.Atoi(fields[3])
	startTicks, _ := strconv.ParseUint(fields[19], 10, 64)
	token := fields[19]
	if boot := linuxBootID(); boot != "" {
		token = boot + ":" + fields[19]
	}
	var uid uint32
	var hasUID bool
	if dirInfo, serr := os.Stat(procDir); serr == nil {
		if st, ok := dirInfo.Sys().(*syscall.Stat_t); ok {
			uid = st.Uid
			hasUID = true
		}
	}
	return linuxProcMeta{
		info: processInfo{
			Alive:      true,
			PGID:       pgid,
			StartToken: token,
			UID:        uid,
			HasUID:     hasUID,
			SID:        sid,
		},
		ppid:       ppid,
		startTicks: startTicks,
	}, nil
}

func inspectProcessErr(pid int) (processInfo, error) {
	meta, err := inspectLinuxProcMeta(pid)
	return meta.info, err
}

func parseLinuxStartToken(token string) (bootID string, ticks uint64, ok bool) {
	if b, t, found := strings.Cut(token, ":"); found {
		v, err := strconv.ParseUint(t, 10, 64)
		if err != nil {
			return "", 0, false
		}
		return b, v, true
	}
	v, err := strconv.ParseUint(token, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return "", v, true
}

func isFDLimitErr(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

func linuxBootUnixSec() (int64, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			sec, perr := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if perr == nil && sec > 0 {
				return sec, true
			}
		}
	}
	return 0, false
}

// verifyProcessFDFree checks PGID, SID, and /proc/<pid> UID ownership using
// syscalls that allocate zero file descriptors, allowing safe verification under EMFILE.
func verifyProcessFDFree(proof processGroupProof, pid int, requireInPGID bool) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	pgid, err := unix.Getpgid(pid)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pgid != proof.Leader.PGID {
		if requireInPGID {
			return false, fmt.Errorf("refusing to signal pid %d: pgid mismatch (%d != %d): %w", pid, pgid, proof.Leader.PGID, ErrProcessIdentityMismatch)
		}
		return false, nil
	}
	if proof.SID > 0 {
		sid, serr := unix.Getsid(pid)
		if errors.Is(serr, unix.ESRCH) {
			return false, nil
		}
		if serr != nil {
			return false, serr
		}
		if sid != proof.SID {
			return false, fmt.Errorf("refusing to signal pid %d: session mismatch: %w", pid, ErrProcessIdentityMismatch)
		}
	}
	if proof.HasUID {
		var st unix.Stat_t
		if uerr := unix.Stat(fmt.Sprintf("/proc/%d", pid), &st); errors.Is(uerr, unix.ENOENT) || errors.Is(uerr, unix.ESRCH) {
			return false, nil
		} else if uerr != nil {
			return false, uerr
		} else if st.Uid != proof.UID {
			return false, fmt.Errorf("refusing to signal pid %d: uid mismatch: %w", pid, ErrProcessIdentityMismatch)
		}
	}
	return true, nil
}

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	return stopVerifiedProcessGroupWithProof(processGroupProof{Leader: id})
}

func stopVerifiedProcessGroupWithProof(proof processGroupProof) error {
	id := proof.Leader
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	// Verify via /proc and signal the process group without allocating a pidfd
	// upfront, so descriptor exhaustion (EMFILE) does not block stopping.
	info, err := inspectProcessErr(id.PID)
	if isFDLimitErr(err) {
		alive, verr := verifyProcessFDFree(proof, id.PID, true)
		if verr != nil {
			return verr
		}
		if !alive {
			return os.ErrProcessDone
		}
		if kerr := syscall.Kill(-id.PGID, syscall.SIGKILL); kerr == nil {
			return nil
		} else if !errors.Is(kerr, syscall.ESRCH) {
			return kerr
		}
		alive2, verr2 := verifyProcessFDFree(proof, id.PID, true)
		if verr2 != nil {
			return verr2
		}
		if !alive2 {
			return os.ErrProcessDone
		}
		if kerr := syscall.Kill(id.PID, syscall.SIGKILL); errors.Is(kerr, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return kerr
		}
	}
	if err != nil {
		return err
	}
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
	info2, err2 := inspectProcessErr(id.PID)
	if isFDLimitErr(err2) {
		alive2, verr2 := verifyProcessFDFree(proof, id.PID, true)
		if verr2 != nil {
			return verr2
		}
		if !alive2 {
			return os.ErrProcessDone
		}
		if kerr := syscall.Kill(id.PID, syscall.SIGKILL); errors.Is(kerr, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return kerr
		}
	}
	if err2 != nil {
		return err2
	}
	if !info2.Alive {
		return os.ErrProcessDone
	}
	if !id.Matches(info2) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	fd, fdErr := unix.PidfdOpen(id.PID, 0)
	if fdErr != nil {
		if errors.Is(fdErr, unix.ESRCH) {
			return os.ErrProcessDone
		}
		if isFDLimitErr(fdErr) {
			if kerr := syscall.Kill(id.PID, syscall.SIGKILL); errors.Is(kerr, syscall.ESRCH) {
				return os.ErrProcessDone
			} else {
				return kerr
			}
		}
		return fdErr
	}
	defer unix.Close(fd)
	if kerr := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); errors.Is(kerr, unix.ESRCH) {
		return os.ErrProcessDone
	} else {
		return kerr
	}
}

func captureVerifiedGroupMembers(proof processGroupProof) map[int]string {
	id := proof.Leader
	if !id.Valid() {
		return nil
	}
	leaderBoot, leaderTicks, hasLeaderStart := parseLinuxStartToken(id.StartToken)
	if !hasLeaderStart {
		return nil
	}
	info, err := inspectProcessErr(id.PID)
	if err != nil || !id.Matches(info) {
		return nil
	}
	members := map[int]string{id.PID: id.StartToken}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return members
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, perr := strconv.Atoi(name)
		if perr != nil || pid <= 0 {
			continue
		}
		meta, merr := inspectLinuxProcMeta(pid)
		if merr != nil || !meta.info.Alive || meta.info.PGID != id.PGID {
			continue
		}
		if pid == id.PID && meta.info.StartToken != id.StartToken {
			continue
		}
		if proof.HasUID && meta.info.HasUID && meta.info.UID != proof.UID {
			continue
		}
		if proof.SID > 0 && meta.info.SID != proof.SID {
			continue
		}
		mBoot, _, _ := parseLinuxStartToken(meta.info.StartToken)
		if leaderBoot != "" && mBoot != "" && mBoot != leaderBoot {
			continue
		}
		if meta.startTicks < leaderTicks {
			continue
		}
		members[pid] = meta.info.StartToken
	}
	return members
}

func stopKnownMembersFDFree(proof processGroupProof, origErr error) error {
	if len(proof.KnownMembers) == 0 {
		return origErr
	}
	verifiedAlive := false
	for pid := range proof.KnownMembers {
		alive, verr := verifyProcessFDFree(proof, pid, false)
		if verr != nil {
			return verr
		}
		if alive {
			verifiedAlive = true
			break
		}
	}
	if !verifiedAlive {
		return os.ErrProcessDone
	}
	if kerr := syscall.Kill(-proof.Leader.PGID, syscall.SIGKILL); kerr == nil {
		return nil
	} else if !errors.Is(kerr, syscall.ESRCH) {
		return kerr
	}
	var lastErr error
	anySignalled := false
	for pid := range proof.KnownMembers {
		alive, verr := verifyProcessFDFree(proof, pid, false)
		if verr != nil {
			return verr
		}
		if !alive {
			continue
		}
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr == nil {
			anySignalled = true
		} else if !errors.Is(kerr, syscall.ESRCH) {
			lastErr = kerr
		}
	}
	if lastErr != nil {
		return lastErr
	}
	if anySignalled {
		return nil
	}
	return os.ErrProcessDone
}

func stopAdoptedProcessGroupAfterExit(proof processGroupProof) error {
	id := proof.Leader
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	leaderBoot, leaderTicks, hasLeaderStart := parseLinuxStartToken(id.StartToken)
	if !hasLeaderStart {
		return ErrProcessIdentityMismatch
	}
	info, err := inspectProcessErr(id.PID)
	if isFDLimitErr(err) {
		return stopKnownMembersFDFree(proof, err)
	}
	if err != nil {
		return err
	}
	if info.Alive && !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	entries, err := os.ReadDir("/proc")
	if isFDLimitErr(err) {
		return stopKnownMembersFDFree(proof, err)
	}
	if err != nil {
		return err
	}
	var bootSec int64
	var hasBoot bool
	if !proof.ExitObservedAt.IsZero() {
		bootSec, hasBoot = linuxBootUnixSec()
	}
	verifiedMembers := 0
	groupVerified := false
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, perr := strconv.Atoi(name)
		if perr != nil || pid <= 0 {
			continue
		}
		meta, merr := inspectLinuxProcMeta(pid)
		if isFDLimitErr(merr) {
			return stopKnownMembersFDFree(proof, merr)
		}
		if merr != nil {
			return merr
		}
		if !meta.info.Alive || meta.info.PGID != id.PGID {
			continue
		}
		if pid == id.PID {
			if meta.info.StartToken != id.StartToken {
				return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
			}
			groupVerified = true
		}
		if proof.HasUID && meta.info.HasUID && meta.info.UID != proof.UID {
			return fmt.Errorf("refusing to signal pgid %d: member pid %d uid mismatch: %w", id.PGID, pid, ErrProcessIdentityMismatch)
		}
		if proof.SID > 0 && meta.info.SID != proof.SID {
			return fmt.Errorf("refusing to signal pgid %d: member pid %d session mismatch: %w", id.PGID, pid, ErrProcessIdentityMismatch)
		}
		mBoot, _, _ := parseLinuxStartToken(meta.info.StartToken)
		if leaderBoot != "" && mBoot != "" && mBoot != leaderBoot {
			return fmt.Errorf("refusing to signal pgid %d: boot mismatch: %w", id.PGID, ErrProcessIdentityMismatch)
		}
		if meta.startTicks < leaderTicks {
			return fmt.Errorf("refusing to signal pgid %d: member pid %d predates job: %w", id.PGID, pid, ErrProcessIdentityMismatch)
		}
		if !proof.ExitObservedAt.IsZero() && hasBoot {
			memberStart := time.Unix(bootSec+int64(meta.startTicks/100), int64((meta.startTicks%100)*10_000_000))
			if memberStart.After(proof.ExitObservedAt.Add(2 * time.Second)) {
				return fmt.Errorf("refusing to signal pgid %d: member pid %d started after leader exit: %w", id.PGID, pid, ErrProcessIdentityMismatch)
			}
		}
		if proof.KnownMembers[pid] == meta.info.StartToken {
			groupVerified = true
		} else if meta.ppid == id.PID {
			groupVerified = true
		} else if parentTok, ok := proof.KnownMembers[meta.ppid]; ok {
			pBoot, pTicks, parsed := parseLinuxStartToken(parentTok)
			if parsed && (leaderBoot == "" || pBoot == "" || pBoot == leaderBoot) && meta.startTicks >= pTicks {
				groupVerified = true
			}
		}
		verifiedMembers++
	}
	if verifiedMembers == 0 {
		return os.ErrProcessDone
	}
	if !groupVerified {
		return fmt.Errorf("refusing to signal unverified pgid %d after leader disappeared: %w", id.PGID, ErrProcessIdentityMismatch)
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
		info, ierr := inspectProcessErr(id.PID)
		if ierr != nil {
			return -1, false, ierr
		}
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
		info, ierr := inspectProcessErr(id.PID)
		if ierr != nil {
			return -1, false, ierr
		}
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
		info, ierr := inspectProcessErr(id.PID)
		if ierr != nil {
			return -1, false, ierr
		}
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
