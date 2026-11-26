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

// verifyPreCapturedMemberFD checks that a pre-verified pidfd still points to the
// same live kernel task in proof.Leader.PGID and matches expectedToken, using 0 FDs.
func verifyPreCapturedMemberFD(proof processGroupProof, pid int, expectedToken string) (hasFD bool, alive bool, err error) {
	entry, ok := proof.verifiedFDs[pid]
	if !ok || entry.fd <= 0 {
		return false, false, nil
	}
	if expectedToken == "" || entry.startToken != expectedToken {
		return true, false, fmt.Errorf("refusing to signal pid %d: %w", pid, ErrProcessIdentityMismatch)
	}
	if kerr := unix.PidfdSendSignal(entry.fd, 0, nil, 0); errors.Is(kerr, unix.ESRCH) {
		return true, false, nil
	} else if kerr != nil {
		return true, false, kerr
	}
	pgid, gerr := unix.Getpgid(pid)
	if errors.Is(gerr, unix.ESRCH) {
		return true, false, nil
	}
	if gerr != nil {
		return true, false, gerr
	}
	if pgid != proof.Leader.PGID {
		return true, false, fmt.Errorf("refusing to signal pid %d: pgid mismatch (%d != %d): %w", pid, pgid, proof.Leader.PGID, ErrProcessIdentityMismatch)
	}
	return true, true, nil
}

func signalVerifiedGroupByFD(proof processGroupProof, origErr error, members map[int]string, allowDoneOnAllExited bool) error {
	anyVerifiedHandle := false
	verifiedAlive := false
	for pid, tok := range members {
		hasFD, alive, verr := verifyPreCapturedMemberFD(proof, pid, tok)
		if verr != nil {
			return verr
		}
		if hasFD {
			anyVerifiedHandle = true
		}
		if alive {
			verifiedAlive = true
			break
		}
	}
	if !anyVerifiedHandle {
		return origErr
	}
	if !verifiedAlive {
		if allowDoneOnAllExited {
			return os.ErrProcessDone
		}
		return origErr
	}
	if kerr := syscall.Kill(-proof.Leader.PGID, syscall.SIGKILL); kerr == nil {
		return nil
	} else if !errors.Is(kerr, syscall.ESRCH) {
		return kerr
	}
	var lastErr error
	anySignalled := false
	for pid, tok := range members {
		hasFD, alive, verr := verifyPreCapturedMemberFD(proof, pid, tok)
		if verr != nil {
			return verr
		}
		if !hasFD || !alive {
			continue
		}
		entry := proof.verifiedFDs[pid]
		if kerr := unix.PidfdSendSignal(entry.fd, unix.SIGKILL, nil, 0); kerr == nil {
			anySignalled = true
		} else if !errors.Is(kerr, unix.ESRCH) {
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
		return signalVerifiedGroupByFD(proof, err, map[int]string{id.PID: id.StartToken}, true)
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
		return signalVerifiedGroupByFD(proof, err2, map[int]string{id.PID: id.StartToken}, true)
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
			return signalVerifiedGroupByFD(proof, fdErr, map[int]string{id.PID: id.StartToken}, true)
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

func captureVerifiedGroupProof(proof *processGroupProof) {
	if proof == nil {
		return
	}
	members := captureVerifiedGroupMembers(*proof)
	if len(members) == 0 {
		return
	}
	proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, members)
	if proof.KnownMembers[proof.Leader.PID] == proof.Leader.StartToken {
		proof.VerifiedAt = time.Now()
	}
	for pid, tok := range members {
		if existing, ok := proof.verifiedFDs[pid]; ok && existing.fd > 0 && existing.startToken == tok {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil || fd <= 0 {
			continue
		}
		// Re-verify StartToken while pidfd is held so the fd is bound to tok.
		info, ierr := inspectProcessErr(pid)
		if ierr != nil || !info.Alive || info.PGID != proof.Leader.PGID || info.StartToken != tok {
			_ = unix.Close(fd)
			continue
		}
		if old, ok := proof.verifiedFDs[pid]; ok && old.fd > 0 {
			_ = unix.Close(old.fd)
		}
		if proof.verifiedFDs == nil {
			proof.verifiedFDs = make(map[int]verifiedProcFD, len(members))
		}
		proof.verifiedFDs[pid] = verifiedProcFD{fd: fd, startToken: tok}
	}
}

func closeVerifiedGroupProof(proof *processGroupProof) {
	if proof == nil {
		return
	}
	for pid, entry := range proof.verifiedFDs {
		if entry.fd > 0 {
			_ = unix.Close(entry.fd)
		}
		delete(proof.verifiedFDs, pid)
	}
}

func processHasJobToken(pid int, token string) bool {
	if pid <= 0 || token == "" {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	target := jobTokenEnvVar + "=" + token
	for _, entry := range strings.Split(string(data), "\x00") {
		if entry == target {
			return true
		}
	}
	return false
}

func stopKnownMembersFDFree(proof processGroupProof, origErr error) error {
	if len(proof.KnownMembers) == 0 {
		return origErr
	}
	return signalVerifiedGroupByFD(proof, origErr, proof.KnownMembers, false)
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
		} else if (proof.JobToken != "" && processHasJobToken(pid, proof.JobToken)) ||
			(!proof.VerifiedAt.IsZero() && id.PGID == id.PID && proof.KnownMembers[id.PID] == id.StartToken) {
			groupVerified = true
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
