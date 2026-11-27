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

func isProcessAccessError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

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

var inspectLinuxProcMetaFn = inspectLinuxProcMeta

func inspectProcessErr(pid int) (processInfo, error) {
	meta, err := inspectLinuxProcMetaFn(pid)
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
	var lastErr error
	anySignalled := false
	for pid, tok := range members {
		hasFD, alive, verr := verifyPreCapturedMemberFD(proof, pid, tok)
		if verr != nil {
			return verr
		}
		if hasFD {
			anyVerifiedHandle = true
		}
		if !hasFD || !alive {
			continue
		}
		verifiedAlive = true
		entry := proof.verifiedFDs[pid]
		if kerr := unix.PidfdSendSignal(entry.fd, unix.SIGKILL, nil, 0); kerr == nil {
			anySignalled = true
		} else if !errors.Is(kerr, unix.ESRCH) {
			lastErr = kerr
		}
	}
	if !anyVerifiedHandle {
		return origErr
	}
	if lastErr != nil {
		return lastErr
	}
	if !verifiedAlive {
		if allowDoneOnAllExited {
			return os.ErrProcessDone
		}
		return origErr
	}
	if !allowDoneOnAllExited {
		// Post-exit descriptor exhaustion could not scan /proc to rule out
		// uncaptured descendants; preserve the retryable FD-limit error after
		// signalling cached verified handles.
		return origErr
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

var readProcDirFn = func() ([]os.DirEntry, error) {
	return os.ReadDir("/proc")
}

func captureVerifiedGroupMembers(proof processGroupProof) map[int]string {
	members, _ := captureVerifiedGroupMembersWithStatus(proof)
	return members
}

func captureVerifiedGroupMembersWithStatus(proof processGroupProof) (map[int]string, bool) {
	id := proof.Leader
	if !id.Valid() {
		return nil, false
	}
	leaderBoot, leaderTicks, hasLeaderStart := parseLinuxStartToken(id.StartToken)
	if !hasLeaderStart {
		return nil, false
	}
	info, err := inspectProcessErr(id.PID)
	if err != nil || !id.Matches(info) {
		return nil, false
	}
	members := map[int]string{id.PID: id.StartToken}
	entries, err := readProcDirFn()
	if err != nil {
		return members, false
	}
	complete := true
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
		if merr != nil {
			complete = false
			if isFDLimitErr(merr) {
				break
			}
			continue
		}
		if !meta.info.Alive || meta.info.PGID != id.PGID {
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
	return members, complete
}

func captureLeaderPipeTargets(pid int) []string {
	if pid <= 0 {
		return nil
	}
	var targets []string
	for _, fd := range []string{"0", "1", "2"} {
		if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd)); err == nil && strings.HasPrefix(target, "pipe:[") {
			targets = mergePipeTargets(targets, []string{target})
		}
	}
	return targets
}

func captureVerifiedGroupProof(proof *processGroupProof) {
	if proof == nil {
		return
	}
	// Prune already-exited kernel pidfds immediately (zero extra FDs) so
	// short-lived children cannot accumulate open descriptors.
	for pid, entry := range proof.verifiedFDs {
		if entry.fd <= 0 {
			delete(proof.verifiedFDs, pid)
			continue
		}
		if kerr := unix.PidfdSendSignal(entry.fd, 0, nil, 0); errors.Is(kerr, unix.ESRCH) {
			_ = unix.Close(entry.fd)
			delete(proof.verifiedFDs, pid)
		}
	}
	members, complete := captureVerifiedGroupMembersWithStatus(*proof)
	if len(members) == 0 {
		return
	}
	// Only prune omitted PIDs when the /proc scan completed cleanly without read
	// errors; under partial reads preserve live cached handles while still closing
	// any handle whose observed StartToken changed.
	for pid, entry := range proof.verifiedFDs {
		liveTok, ok := members[pid]
		if (ok && liveTok != entry.startToken) || (!ok && complete) {
			if entry.fd > 0 {
				_ = unix.Close(entry.fd)
			}
			delete(proof.verifiedFDs, pid)
		}
	}
	proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, members)
	if proof.KnownMembers[proof.Leader.PID] == proof.Leader.StartToken {
		proof.VerifiedAt = time.Now()
		if pipes := captureLeaderPipeTargets(proof.Leader.PID); len(pipes) > 0 {
			proof.LeaderPipeTargets = mergePipeTargets(proof.LeaderPipeTargets, pipes)
		}
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

func processHasJobTokenErr(pid int, token string) (bool, error) {
	if pid <= 0 || token == "" {
		return false, nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false, err
	}
	target := jobTokenEnvVar + "=" + token
	for _, entry := range strings.Split(string(data), "\x00") {
		if entry == target {
			return true, nil
		}
	}
	return false, nil
}

func processHasInheritedFDMatchErr(pid int, outputFile string, pipeTargets []string) (bool, error) {
	if pid <= 0 || (outputFile == "" && len(pipeTargets) == 0) {
		return false, nil
	}
	var pipeSet map[string]struct{}
	if len(pipeTargets) > 0 {
		pipeSet = make(map[string]struct{}, len(pipeTargets))
		for _, t := range pipeTargets {
			if strings.HasPrefix(t, "pipe:[") {
				pipeSet[t] = struct{}{}
			}
		}
	}
	var outStat os.FileInfo
	var outErr error
	if outputFile != "" {
		outStat, outErr = os.Stat(outputFile)
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	deletedTarget := ""
	if outputFile != "" {
		deletedTarget = outputFile + " (deleted)"
	}
	for _, entry := range entries {
		fdPath := fdDir + "/" + entry.Name()
		if outputFile != "" && outErr == nil {
			if st, serr := os.Stat(fdPath); serr == nil && os.SameFile(outStat, st) {
				return true, nil
			}
		}
		if target, lerr := os.Readlink(fdPath); lerr == nil {
			if outputFile != "" && (target == outputFile || target == deletedTarget) {
				return true, nil
			}
			if _, ok := pipeSet[target]; ok {
				return true, nil
			}
		}
	}
	return false, nil
}

func stopKnownMembersFDFree(proof processGroupProof, origErr error) error {
	if len(proof.KnownMembers) == 0 {
		return origErr
	}
	return signalVerifiedGroupByFD(proof, origErr, proof.KnownMembers, false)
}

type linuxGroupCandidate struct {
	pid      int
	meta     linuxProcMeta
	verified bool
}

func collectAndVerifyLinuxCandidates(proof *processGroupProof, id ProcessIdentityInfo, leaderBoot string, leaderTicks uint64) (candidates []linuxGroupCandidate, handleErr error, mismatchErr error) {
	entries, err := readProcDirFn()
	if err != nil {
		return nil, err, nil
	}
	// liveVerified records only members positively verified in this scan so that
	// ancestry propagation never trusts stale or unreadable root/parent tokens.
	liveVerified := make(map[int]string)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, perr := strconv.Atoi(name)
		if perr != nil || pid <= 0 {
			continue
		}
		meta, merr := inspectLinuxProcMetaFn(pid)
		if merr != nil {
			if isProcessAccessError(merr) {
				if pid == id.PID || proof.KnownMembers[pid] != "" {
					handleErr = merr
				}
				continue
			}
			handleErr = merr
			break
		}
		if !meta.info.Alive || meta.info.PGID != id.PGID {
			continue
		}
		verified := false
		if pid == id.PID {
			if meta.info.StartToken != id.StartToken {
				return nil, nil, fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
			}
			verified = true
		}
		if proof.HasUID && meta.info.HasUID && meta.info.UID != proof.UID {
			return nil, nil, fmt.Errorf("refusing to signal pgid %d: member pid %d uid mismatch: %w", id.PGID, pid, ErrProcessIdentityMismatch)
		}
		if proof.SID > 0 && meta.info.SID != proof.SID {
			return nil, nil, fmt.Errorf("refusing to signal pgid %d: member pid %d session mismatch: %w", id.PGID, pid, ErrProcessIdentityMismatch)
		}
		mBoot, _, _ := parseLinuxStartToken(meta.info.StartToken)
		if leaderBoot != "" && mBoot != "" && mBoot != leaderBoot {
			return nil, nil, fmt.Errorf("refusing to signal pgid %d: boot mismatch: %w", id.PGID, ErrProcessIdentityMismatch)
		}
		if meta.startTicks < leaderTicks {
			return nil, nil, fmt.Errorf("refusing to signal pgid %d: member pid %d predates job: %w", id.PGID, pid, ErrProcessIdentityMismatch)
		}
		if proof.KnownMembers[pid] == meta.info.StartToken {
			verified = true
		}
		if !verified && proof.JobToken != "" {
			hasTok, tokErr := processHasJobTokenErr(pid, proof.JobToken)
			if isFDLimitErr(tokErr) {
				handleErr = tokErr
				break
			}
			if hasTok {
				verified = true
			}
		}
		if !verified && (proof.OutputFile != "" || len(proof.LeaderPipeTargets) > 0) {
			hasFD, fdErr := processHasInheritedFDMatchErr(pid, proof.OutputFile, proof.LeaderPipeTargets)
			if isFDLimitErr(fdErr) {
				handleErr = fdErr
				break
			}
			if hasFD {
				verified = true
			}
		}
		if verified {
			liveVerified[pid] = meta.info.StartToken
			if proof.KnownMembers == nil {
				proof.KnownMembers = make(map[int]string)
			}
			if proof.KnownMembers[pid] == "" {
				proof.KnownMembers[pid] = meta.info.StartToken
			}
		}
		candidates = append(candidates, linuxGroupCandidate{pid: pid, meta: meta, verified: verified})
	}
	// Resolve multi-hop parent ancestry transitively using only live verified
	// parent instances from this scan, persisting newly verified identities.
	for {
		added := false
		for i := range candidates {
			c := &candidates[i]
			if c.verified {
				continue
			}
			if parentTok, ok := liveVerified[c.meta.ppid]; ok {
				pBoot, pTicks, parsed := parseLinuxStartToken(parentTok)
				if parsed && (leaderBoot == "" || pBoot == "" || pBoot == leaderBoot) && c.meta.startTicks >= pTicks {
					c.verified = true
					liveVerified[c.pid] = c.meta.info.StartToken
					if proof.KnownMembers == nil {
						proof.KnownMembers = make(map[int]string)
					}
					if proof.KnownMembers[c.pid] == "" {
						proof.KnownMembers[c.pid] = c.meta.info.StartToken
					}
					added = true
				}
			}
		}
		if !added {
			break
		}
	}
	return candidates, handleErr, nil
}

func hasVerifiedLiveKnownMemberLinux(proof processGroupProof) (bool, error) {
	id := proof.Leader
	var inspectErr error
	for pid, tok := range proof.KnownMembers {
		if pid <= 0 || pid == id.PID || tok == "" {
			continue
		}
		hasFD, alive, verr := verifyPreCapturedMemberFD(proof, pid, tok)
		if hasFD {
			if verr == nil && alive {
				return true, nil
			}
			continue
		}
		meta, merr := inspectLinuxProcMetaFn(pid)
		if merr != nil {
			if inspectErr == nil {
				inspectErr = merr
			}
			continue
		}
		if !meta.info.Alive || meta.info.PGID != id.PGID || meta.info.StartToken != tok {
			continue
		}
		if proof.HasUID && meta.info.HasUID && meta.info.UID != proof.UID {
			continue
		}
		if proof.SID > 0 && meta.info.SID != proof.SID {
			continue
		}
		return true, nil
	}
	return false, inspectErr
}

func signalVerifiedKnownMembersLinux(proof processGroupProof, id ProcessIdentityInfo) (anySignalled bool, fdLimited bool, err error) {
	var firstErr error
	for pid, tok := range proof.KnownMembers {
		if pid <= 0 || pid == id.PID || tok == "" {
			continue
		}
		hasFD, alive, verr := verifyPreCapturedMemberFD(proof, pid, tok)
		if hasFD {
			if verr == nil && alive {
				entry := proof.verifiedFDs[pid]
				if kerr := unix.PidfdSendSignal(entry.fd, unix.SIGKILL, nil, 0); kerr == nil {
					anySignalled = true
				} else if !errors.Is(kerr, unix.ESRCH) && firstErr == nil {
					firstErr = kerr
				}
			}
			continue
		}
		fd, ferr := unix.PidfdOpen(pid, 0)
		if isFDLimitErr(ferr) {
			return anySignalled, true, ferr
		}
		if errors.Is(ferr, unix.ESRCH) {
			continue
		}
		if ferr != nil {
			if firstErr == nil {
				firstErr = ferr
			}
			continue
		}
		meta, merr := inspectLinuxProcMetaFn(pid)
		if merr == nil && meta.info.Alive && meta.info.PGID == id.PGID && meta.info.StartToken == tok &&
			(!proof.HasUID || !meta.info.HasUID || meta.info.UID == proof.UID) &&
			(proof.SID <= 0 || meta.info.SID == proof.SID) {
			if kerr := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); kerr == nil {
				anySignalled = true
			} else if !errors.Is(kerr, unix.ESRCH) && firstErr == nil {
				firstErr = kerr
			}
		} else if isFDLimitErr(merr) {
			_ = unix.Close(fd)
			return anySignalled, true, merr
		} else if merr != nil && !errors.Is(merr, unix.ESRCH) && firstErr == nil {
			firstErr = merr
		}
		_ = unix.Close(fd)
	}
	return anySignalled, false, firstErr
}

func hasVerifiedLiveGroupMember(proof processGroupProof) (bool, error) {
	id := proof.Leader
	if !id.Valid() {
		return false, ErrProcessIdentityMismatch
	}
	leaderBoot, leaderTicks, hasLeaderStart := parseLinuxStartToken(id.StartToken)
	if !hasLeaderStart {
		return false, ErrProcessIdentityMismatch
	}
	cloned := cloneGroupProof(proof)
	candidates, handleErr, mismatchErr := collectAndVerifyLinuxCandidates(&cloned, id, leaderBoot, leaderTicks)
	if mismatchErr != nil {
		if ok, _ := hasVerifiedLiveKnownMemberLinux(proof); ok {
			return true, nil
		}
		return false, mismatchErr
	}
	for _, c := range candidates {
		if c.verified {
			return true, nil
		}
	}
	if ok, _ := hasVerifiedLiveKnownMemberLinux(proof); ok {
		return true, nil
	}
	if handleErr != nil {
		return false, handleErr
	}
	if len(candidates) > 0 && canRetryIncompleteGroupProof(proof) {
		return true, nil
	}
	return false, nil
}

func stopAdoptedProcessGroupAfterExit(proof processGroupProof) error {
	cloned := cloneGroupProof(proof)
	cloned.verifiedFDs = proof.verifiedFDs
	return stopAdoptedProcessGroupAfterExitTrack(&cloned)
}

func stopAdoptedProcessGroupAfterExitTrack(proof *processGroupProof) error {
	if proof == nil {
		return ErrProcessIdentityMismatch
	}
	id := proof.Leader
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	leaderBoot, leaderTicks, hasLeaderStart := parseLinuxStartToken(id.StartToken)
	if !hasLeaderStart {
		return ErrProcessIdentityMismatch
	}
	info, leaderErr := inspectProcessErrFn(id.PID)
	if isFDLimitErr(leaderErr) {
		return stopKnownMembersFDFree(*proof, leaderErr)
	}
	if leaderErr != nil && !isProcessAccessError(leaderErr) {
		return leaderErr
	}
	if leaderErr == nil && info.Alive && !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	var bootSec int64
	var hasBoot bool
	if !proof.ExitObservedAt.IsZero() {
		bootSec, hasBoot = linuxBootUnixSec()
	}

	// Keep KnownMembers mutable on a cloned copy across passes and retain any newly
	// verified identities back into proof.KnownMembers even if a pass fails mid-way.
	scanProof := cloneGroupProof(*proof)
	scanProof.verifiedFDs = proof.verifiedFDs
	if scanProof.KnownMembers == nil {
		scanProof.KnownMembers = make(map[int]string)
	}
	defer func() {
		proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, scanProof.KnownMembers)
	}()

	const maxPasses = 32
	deadline := time.Now().Add(2 * time.Second)
	anySignalled := false
	consecutiveEmpty := 0
	lastLiveMembers := 0
	for pass := 0; pass < maxPasses; pass++ {
		signalled, empty, liveMembers, perr := stopAdoptedProcessGroupAfterExitPass(&scanProof, id, leaderBoot, leaderTicks, bootSec, hasBoot)
		lastLiveMembers = liveMembers
		if signalled {
			anySignalled = true
		}
		if perr != nil {
			if errors.Is(perr, ErrProcessIdentityMismatch) {
				sig, fdLimited, serr := signalVerifiedKnownMembersLinux(*proof, id)
				if fdLimited {
					return stopKnownMembersFDFree(*proof, serr)
				}
				if leaderErr != nil && isProcessAccessError(leaderErr) {
					return leaderErr
				}
				if sig {
					// Brief grace wait so signalled persisted children exit cleanly.
					for i := 0; i < 20; i++ {
						live, _ := hasVerifiedLiveKnownMemberLinux(*proof)
						if !live {
							return nil
						}
						time.Sleep(5 * time.Millisecond)
					}
					if serr != nil {
						return serr
					}
					return fmt.Errorf("verified process group members for pid %d still alive after signalling: %w", id.PID, errGroupMembersStillAlive)
				}
				if serr != nil {
					return serr
				}
				return perr
			}
			if isProcessAccessError(perr) {
				sig, fdLimited, serr := signalVerifiedKnownMembersLinux(*proof, id)
				if fdLimited {
					return stopKnownMembersFDFree(*proof, serr)
				}
				_ = sig
				_ = serr
				if leaderErr != nil && isProcessAccessError(leaderErr) {
					return leaderErr
				}
			}
			return perr
		}
		if empty {
			consecutiveEmpty++
			// Require two consecutive clean empty scans so a member listed in
			// /proc that forked and exited before its metadata was read cannot
			// leave a descendant undetected.
			if consecutiveEmpty >= 2 {
				if leaderErr != nil {
					_ = stopKnownMembersFDFree(*proof, leaderErr)
					return leaderErr
				}
				if anySignalled {
					return nil
				}
				return os.ErrProcessDone
			}
		} else {
			consecutiveEmpty = 0
		}
		if pass == maxPasses-1 || time.Now().After(deadline) {
			break
		}
		if signalled {
			time.Sleep(10 * time.Millisecond)
		} else if empty {
			time.Sleep(2 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return fmt.Errorf("pgid %d still has %d live member(s) after signalling: %w", id.PGID, lastLiveMembers, errGroupMembersStillAlive)
}

func stopAdoptedProcessGroupAfterExitPass(proof *processGroupProof, id ProcessIdentityInfo, leaderBoot string, leaderTicks uint64, bootSec int64, hasBoot bool) (signalled bool, empty bool, liveCount int, err error) {
	candidates, handleErr, mismatchErr := collectAndVerifyLinuxCandidates(proof, id, leaderBoot, leaderTicks)
	if mismatchErr != nil {
		return false, false, 0, mismatchErr
	}
	if handleErr != nil && len(candidates) == 0 {
		if isFDLimitErr(handleErr) {
			return false, false, 0, stopKnownMembersFDFree(*proof, handleErr)
		}
		if isProcessAccessError(handleErr) {
			sig, _, serr := signalVerifiedKnownMembersLinux(*proof, id)
			if serr != nil {
				return sig, false, 0, serr
			}
			return sig, false, 0, handleErr
		}
		return false, false, 0, handleErr
	}

	liveMembers := 0
	unverifiedMembers := 0
	goneMembers := 0
	groupVerified := false
	var verifiedFDs []int
	var openedFDs []int
	defer func() {
		for _, fd := range openedFDs {
			_ = unix.Close(fd)
		}
	}()
	for _, cand := range candidates {
		pid := cand.pid
		meta := cand.meta
		if !cand.verified && !proof.ExitObservedAt.IsZero() && hasBoot {
			memberStart := time.Unix(bootSec+int64(meta.startTicks/100), int64((meta.startTicks%100)*10_000_000))
			if memberStart.After(proof.ExitObservedAt.Add(2 * time.Second)) {
				return false, false, liveMembers, fmt.Errorf("refusing to signal pgid %d: member pid %d started after leader exit: %w", id.PGID, pid, ErrProcessIdentityMismatch)
			}
		}
		liveMembers++
		if !cand.verified {
			unverifiedMembers++
			continue
		}
		groupVerified = true
		if existing, ok := proof.verifiedFDs[pid]; ok && existing.fd > 0 && existing.startToken == meta.info.StartToken {
			verifiedFDs = append(verifiedFDs, existing.fd)
			continue
		}
		if isFDLimitErr(handleErr) {
			continue
		}
		fd, ferr := unix.PidfdOpen(pid, 0)
		if isFDLimitErr(ferr) {
			handleErr = ferr
			continue
		}
		if errors.Is(ferr, unix.ESRCH) {
			goneMembers++
			continue
		}
		if ferr != nil {
			if handleErr == nil {
				handleErr = ferr
			}
			continue
		}
		// Revalidate opened pidfd handle before accepting it.
		meta2, merr := inspectLinuxProcMeta(pid)
		if merr != nil {
			_ = unix.Close(fd)
			if isFDLimitErr(merr) {
				handleErr = merr
				continue
			}
			if handleErr == nil {
				handleErr = merr
			}
			continue
		}
		if !meta2.info.Alive || meta2.info.PGID != id.PGID {
			_ = unix.Close(fd)
			goneMembers++
			continue
		}
		if meta2.info.StartToken != meta.info.StartToken {
			_ = unix.Close(fd)
			return false, false, liveMembers, fmt.Errorf("refusing to signal pid %d: %w", pid, ErrProcessIdentityMismatch)
		}
		openedFDs = append(openedFDs, fd)
		verifiedFDs = append(verifiedFDs, fd)
	}

	var sigErr error
	anySignalled := false
	for _, fd := range verifiedFDs {
		if kerr := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); kerr == nil {
			anySignalled = true
		} else if !errors.Is(kerr, unix.ESRCH) && sigErr == nil {
			sigErr = kerr
		}
	}

	if handleErr != nil {
		if isFDLimitErr(handleErr) {
			if fdErr := stopKnownMembersFDFree(*proof, handleErr); fdErr != nil {
				return anySignalled, false, liveMembers, fdErr
			}
		}
		return anySignalled, false, liveMembers, handleErr
	}
	if liveMembers == 0 {
		return false, true, 0, nil
	}
	if !groupVerified || unverifiedMembers > 0 {
		if canRetryIncompleteGroupProof(*proof) {
			return anySignalled, false, liveMembers, fmt.Errorf("incomplete verification proof for tokenless pgid %d (%d unverified live member(s)): %w", id.PGID, unverifiedMembers, errIncompleteGroupProof)
		}
		return anySignalled, false, liveMembers, fmt.Errorf("refusing to signal unverified pgid %d after leader disappeared: %w", id.PGID, ErrProcessIdentityMismatch)
	}
	if sigErr != nil {
		return anySignalled, false, liveMembers, sigErr
	}
	if len(verifiedFDs)+goneMembers != liveMembers {
		return anySignalled, false, liveMembers, fmt.Errorf("incomplete cleanup for pgid %d (%d/%d members accounted): %w", id.PGID, len(verifiedFDs)+goneMembers, liveMembers, ErrProcessIdentityMismatch)
	}
	return anySignalled, false, liveMembers, nil
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
