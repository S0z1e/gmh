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

const darwinNoteExitStatus = 0x04000000

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

var darwinSysctlKinfoProcSliceFn = unix.SysctlKinfoProcSlice

func inspectProcess(pid int) processInfo {
	info, _ := inspectProcessErr(pid)
	return info
}

func inspectProcessErr(pid int) (processInfo, error) {
	if pid <= 0 {
		return processInfo{}, nil
	}
	procs, err := darwinSysctlKinfoProcSliceFn("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return processInfo{}, nil
		}
		return processInfo{}, err
	}
	if len(procs) == 0 {
		return processInfo{}, nil
	}
	proc := procs[0]
	if int(proc.Proc.P_pid) != pid {
		return processInfo{}, nil
	}
	if proc.Proc.P_stat == 5 { // SZOMB in Darwin's sys/proc.h.
		return processInfo{Zombie: true}, nil
	}
	if proc.Proc.P_starttime.Sec <= 0 && proc.Proc.P_starttime.Usec <= 0 {
		return processInfo{}, nil
	}
	sid, _ := unix.Getsid(pid)
	return processInfo{
		Alive:      true,
		PGID:       int(proc.Eproc.Pgid),
		StartToken: fmt.Sprintf("%d:%d", proc.Proc.P_starttime.Sec, proc.Proc.P_starttime.Usec),
		UID:        proc.Eproc.Ucred.Uid,
		HasUID:     true,
		SID:        sid,
		Sess:       proc.Eproc.Sess,
	}, nil
}

func parseDarwinStartTime(token string) (sec int64, usec int32, ok bool) {
	secStr, usecStr, found := strings.Cut(token, ":")
	if !found {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(secStr, 10, 64)
	u, err2 := strconv.ParseInt(usecStr, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return s, int32(u), true
}

func darwinStartTimeBefore(sec int64, usec int32, otherSec int64, otherUsec int32) bool {
	return sec < otherSec || (sec == otherSec && usec < otherUsec)
}

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	return stopVerifiedProcessGroupWithProof(processGroupProof{Leader: id})
}

func stopVerifiedProcessGroupWithProof(proof processGroupProof) error {
	id := proof.Leader
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	info, err := inspectProcessErrFn(id.PID)
	if err != nil {
		return err
	}
	if !info.Alive {
		return os.ErrProcessDone
	}
	if !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	return darwinResolveGroupKillError(id.PGID, syscall.Kill(-id.PGID, syscall.SIGKILL))
}

func captureVerifiedGroupMembers(proof processGroupProof) map[int]string {
	id := proof.Leader
	if !id.Valid() {
		return nil
	}
	leaderSec, leaderUsec, hasLeaderStart := parseDarwinStartTime(id.StartToken)
	if !hasLeaderStart {
		return nil
	}
	info := inspectProcess(id.PID)
	if !id.Matches(info) {
		return nil
	}
	procs, err := darwinSysctlKinfoProcSliceFn("kern.proc.pgrp", id.PGID)
	if err != nil {
		return map[int]string{id.PID: id.StartToken}
	}
	members := map[int]string{id.PID: id.StartToken}
	for _, p := range procs {
		if p.Proc.P_stat == 5 {
			continue
		}
		pid := int(p.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		if proof.HasUID && p.Eproc.Ucred.Uid != proof.UID {
			continue
		}
		if proof.Sess != 0 && p.Eproc.Sess != proof.Sess {
			continue
		}
		if proof.SID > 0 {
			if msid, serr := unix.Getsid(pid); serr == nil && msid != proof.SID {
				continue
			}
		}
		pSec, pUsec := int64(p.Proc.P_starttime.Sec), int32(p.Proc.P_starttime.Usec)
		if darwinStartTimeBefore(pSec, pUsec, leaderSec, leaderUsec) {
			continue
		}
		tok := fmt.Sprintf("%d:%d", pSec, pUsec)
		if pid == id.PID && tok != id.StartToken {
			continue
		}
		members[pid] = tok
	}
	return members
}

func captureVerifiedGroupProof(proof *processGroupProof) {
	if proof == nil {
		return
	}
	members := captureVerifiedGroupMembers(*proof)
	if len(members) > 0 {
		proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, members)
		if proof.KnownMembers[proof.Leader.PID] == proof.Leader.StartToken {
			proof.VerifiedAt = time.Now()
		}
	}
}

func closeVerifiedGroupProof(proof *processGroupProof) {}

func darwinResolveGroupKillError(pgid int, err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if !errors.Is(err, unix.EPERM) {
		return err
	}
	// On XNU, signalling a process group whose remaining members just entered
	// teardown or were reaped can fail with EPERM instead of ESRCH.
	procs, queryErr := darwinSysctlKinfoProcSliceFn("kern.proc.pgrp", pgid)
	if errors.Is(queryErr, unix.ESRCH) {
		return os.ErrProcessDone
	}
	if queryErr != nil {
		return err
	}
	const darwinPWexit = 0x00002000
	for _, p := range procs {
		if p.Proc.P_stat != 5 && (p.Proc.P_flag&darwinPWexit) == 0 {
			return err
		}
	}
	return os.ErrProcessDone
}

func darwinResolveGroupCandidates(proof processGroupProof, id ProcessIdentityInfo, leaderSec int64, leaderUsec int32, procs []unix.KinfoProc) (known map[int]string, liveCount int, err error) {
	known = mergeGroupMembers(nil, proof.KnownMembers)
	eligible := make(map[int]bool, len(procs))
	inProcs := make(map[int]bool, len(procs))
	var mismatchErr error
	for _, p := range procs {
		if p.Proc.P_stat == 5 {
			continue
		}
		liveCount++
		pid := int(p.Proc.P_pid)
		inProcs[pid] = true
		pSec, pUsec := int64(p.Proc.P_starttime.Sec), int32(p.Proc.P_starttime.Usec)
		tok := fmt.Sprintf("%d:%d", pSec, pUsec)
		if pid == id.PID {
			if tok != id.StartToken {
				return nil, liveCount, fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
			}
			known[pid] = tok
		}
		if proof.HasUID && p.Eproc.Ucred.Uid != proof.UID {
			recordFirstMismatch(&mismatchErr, fmt.Errorf("refusing to signal pgid %d: member pid %d uid mismatch: %w", id.PGID, pid, ErrProcessIdentityMismatch))
			continue
		}
		if proof.Sess != 0 && p.Eproc.Sess != proof.Sess {
			recordFirstMismatch(&mismatchErr, fmt.Errorf("refusing to signal pgid %d: member pid %d session mismatch: %w", id.PGID, pid, ErrProcessIdentityMismatch))
			continue
		}
		if proof.SID > 0 {
			if msid, serr := unix.Getsid(pid); serr == nil && msid != proof.SID {
				recordFirstMismatch(&mismatchErr, fmt.Errorf("refusing to signal pgid %d: member pid %d sid mismatch: %w", id.PGID, pid, ErrProcessIdentityMismatch))
				continue
			}
		}
		if darwinStartTimeBefore(pSec, pUsec, leaderSec, leaderUsec) {
			recordFirstMismatch(&mismatchErr, fmt.Errorf("refusing to signal pgid %d: member pid %d predates job: %w", id.PGID, pid, ErrProcessIdentityMismatch))
			continue
		}
		eligible[pid] = true
	}
	// Resolve descendant ancestry transitively so multi-hop children forked after
	// leader exit (c1 -> c2 -> c3) inherit verification across hops.
	for {
		added := false
		for _, p := range procs {
			if p.Proc.P_stat == 5 {
				continue
			}
			pid := int(p.Proc.P_pid)
			if !eligible[pid] {
				continue
			}
			pSec, pUsec := int64(p.Proc.P_starttime.Sec), int32(p.Proc.P_starttime.Usec)
			tok := fmt.Sprintf("%d:%d", pSec, pUsec)
			if existing, ok := known[pid]; ok && existing == tok {
				continue
			}
			ppid := int(p.Eproc.Ppid)
			if ppid == id.PID {
				known[pid] = tok
				added = true
			} else if parentTok, ok := known[ppid]; ok && (!inProcs[ppid] || eligible[ppid]) {
				parentSec, parentUsec, parsed := parseDarwinStartTime(parentTok)
				if parsed && !darwinStartTimeBefore(pSec, pUsec, parentSec, parentUsec) {
					known[pid] = tok
					added = true
				}
			}
		}
		if !added {
			break
		}
	}
	return known, liveCount, mismatchErr
}

func darwinMatchesKnownMember(proof processGroupProof, id ProcessIdentityInfo, info processInfo, tok string) bool {
	if !info.Alive || info.Zombie || info.PGID != id.PGID || info.StartToken != tok {
		return false
	}
	if proof.HasUID && info.HasUID && info.UID != proof.UID {
		return false
	}
	if proof.Sess != 0 && info.Sess != proof.Sess {
		return false
	}
	return true
}

func hasVerifiedLiveKnownMemberDarwin(proof processGroupProof) (bool, error) {
	id := proof.Leader
	var inspectErr error
	for pid, tok := range proof.KnownMembers {
		if pid <= 0 || pid == id.PID || tok == "" {
			continue
		}
		info, err := inspectProcessErrFn(pid)
		if err != nil {
			if inspectErr == nil {
				inspectErr = err
			}
			continue
		}
		if darwinMatchesKnownMember(proof, id, info, tok) {
			return true, nil
		}
	}
	return false, inspectErr
}

func hasVerifiedLiveGroupMember(proof processGroupProof) (bool, error) {
	id := proof.Leader
	if !id.Valid() {
		return false, ErrProcessIdentityMismatch
	}
	leaderSec, leaderUsec, hasLeaderStart := parseDarwinStartTime(id.StartToken)
	if !hasLeaderStart {
		return false, ErrProcessIdentityMismatch
	}
	procs, queryErr := darwinSysctlKinfoProcSliceFn("kern.proc.pgrp", id.PGID)
	if errors.Is(queryErr, unix.ESRCH) {
		return false, nil
	}
	if queryErr != nil {
		if live, lerr := hasVerifiedLiveKnownMemberDarwin(proof); live {
			return true, nil
		} else if lerr != nil {
			return false, lerr
		}
		return false, queryErr
	}
	known, liveCount, err := darwinResolveGroupCandidates(proof, id, leaderSec, leaderUsec, procs)
	if err != nil {
		scanProof := cloneGroupProof(proof)
		scanProof.KnownMembers = mergeGroupMembers(scanProof.KnownMembers, known)
		if live, lerr := hasVerifiedLiveKnownMemberDarwin(scanProof); live {
			return true, nil
		} else if lerr != nil {
			return false, lerr
		}
		return false, err
	}
	if liveCount == 0 {
		return false, nil
	}
	for _, p := range procs {
		if p.Proc.P_stat == 5 {
			continue
		}
		pid := int(p.Proc.P_pid)
		pSec, pUsec := int64(p.Proc.P_starttime.Sec), int32(p.Proc.P_starttime.Usec)
		tok := fmt.Sprintf("%d:%d", pSec, pUsec)
		if known[pid] == tok {
			return true, nil
		}
	}
	if !proof.VerifiedAt.IsZero() && proof.SID > 0 && proof.SID == id.PID && id.PGID == id.PID && proof.KnownMembers[id.PID] == id.StartToken {
		return true, nil
	}
	if canRetryIncompleteGroupProof(proof) {
		return true, nil
	}
	return false, nil
}

func stopAdoptedProcessGroupAfterExit(proof processGroupProof) error {
	cloned := cloneGroupProof(proof)
	return stopAdoptedProcessGroupAfterExitTrack(&cloned)
}

func darwinSignalVerifiedKnownMembers(proof processGroupProof, id ProcessIdentityInfo) (anySignalled bool, err error) {
	var firstErr error
	for pid, tok := range proof.KnownMembers {
		if pid <= 0 || pid == id.PID || tok == "" {
			continue
		}
		info, ierr := inspectProcessErrFn(pid)
		if ierr != nil {
			if firstErr == nil && !errors.Is(ierr, os.ErrProcessDone) {
				firstErr = ierr
			}
			continue
		}
		if !darwinMatchesKnownMember(proof, id, info, tok) {
			continue
		}
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr == nil {
			anySignalled = true
		} else if !errors.Is(kerr, syscall.ESRCH) && firstErr == nil {
			firstErr = kerr
		}
	}
	return anySignalled, firstErr
}

func darwinCleanupKnownMembersOnMismatch(proof *processGroupProof, id ProcessIdentityInfo, leaderErr, mismatchErr error) error {
	sig, serr := darwinSignalVerifiedKnownMembers(*proof, id)
	if leaderErr != nil && isProcessAccessError(leaderErr) {
		return leaderErr
	}
	if sig {
		return waitForKnownMembersExit(id, serr, func() (bool, error) {
			return hasVerifiedLiveKnownMemberDarwin(*proof)
		})
	}
	if serr != nil {
		return serr
	}
	return mismatchErr
}

func stopAdoptedProcessGroupAfterExitTrack(proof *processGroupProof) error {
	if proof == nil {
		return ErrProcessIdentityMismatch
	}
	id := proof.Leader
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	leaderSec, leaderUsec, hasLeaderStart := parseDarwinStartTime(id.StartToken)
	if !hasLeaderStart {
		return ErrProcessIdentityMismatch
	}
	info, leaderErr := inspectProcessErrFn(id.PID)
	if leaderErr != nil && !isProcessAccessError(leaderErr) {
		return leaderErr
	}
	if leaderErr == nil && info.Alive && !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	procs, queryErr := darwinSysctlKinfoProcSliceFn("kern.proc.pgrp", id.PGID)
	if errors.Is(queryErr, unix.ESRCH) {
		if leaderErr != nil {
			_, _ = darwinSignalVerifiedKnownMembers(*proof, id)
			return leaderErr
		}
		return os.ErrProcessDone
	}
	if queryErr != nil {
		if isProcessAccessError(queryErr) {
			_, _ = darwinSignalVerifiedKnownMembers(*proof, id)
		}
		return queryErr
	}

	known, liveCount, cerr := darwinResolveGroupCandidates(*proof, id, leaderSec, leaderUsec, procs)
	proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, known)
	if cerr != nil {
		return darwinCleanupKnownMembersOnMismatch(proof, id, leaderErr, cerr)
	}
	if liveCount == 0 {
		if leaderErr != nil {
			_, _ = darwinSignalVerifiedKnownMembers(*proof, id)
			return leaderErr
		}
		return os.ErrProcessDone
	}

	groupVerified := false
	var postExitMismatchErr error
	hasSetsidProof := !proof.VerifiedAt.IsZero() && proof.SID > 0 && proof.SID == id.PID && id.PGID == id.PID && proof.KnownMembers[id.PID] == id.StartToken
	for _, p := range procs {
		if p.Proc.P_stat == 5 {
			continue
		}
		pid := int(p.Proc.P_pid)
		pSec, pUsec := int64(p.Proc.P_starttime.Sec), int32(p.Proc.P_starttime.Usec)
		tok := fmt.Sprintf("%d:%d", pSec, pUsec)
		directVerified := known[pid] == tok
		if !directVerified && !proof.ExitObservedAt.IsZero() {
			memberStart := time.Unix(pSec, int64(pUsec)*1000)
			if memberStart.After(proof.ExitObservedAt.Add(10 * time.Millisecond)) {
				recordFirstMismatch(&postExitMismatchErr, fmt.Errorf("refusing to signal pgid %d: member pid %d started after leader exit: %w", id.PGID, pid, ErrProcessIdentityMismatch))
			}
		}
		if directVerified || hasSetsidProof {
			groupVerified = true
		}
	}
	if postExitMismatchErr != nil {
		return darwinCleanupKnownMembersOnMismatch(proof, id, leaderErr, postExitMismatchErr)
	}
	if !groupVerified {
		if canRetryIncompleteGroupProof(*proof) {
			return fmt.Errorf("incomplete verification proof for tokenless pgid %d: %w", id.PGID, errIncompleteGroupProof)
		}
		return fmt.Errorf("refusing to signal unverified pgid %d after leader disappeared: %w", id.PGID, ErrProcessIdentityMismatch)
	}
	kerr := darwinResolveGroupKillError(id.PGID, syscall.Kill(-id.PGID, syscall.SIGKILL))
	if kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		if isProcessAccessError(kerr) {
			_, _ = darwinSignalVerifiedKnownMembers(*proof, id)
		}
		return kerr
	}
	if leaderErr != nil {
		return leaderErr
	}
	return kerr
}

func verifyBeforeWait(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	info, err := inspectProcessErrFn(id.PID)
	if err != nil {
		return err
	}
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
