package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func isWindowsProcessMissing(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

func processGroupID(pid int) int {
	if pid <= 0 {
		return 0
	}
	return pid
}

func inspectProcess(pid int) processInfo {
	if pid <= 0 {
		return processInfo{}
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processInfo{}
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != 259 { // STILL_ACTIVE
		return processInfo{}
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return processInfo{}
	}
	return processInfo{
		Alive:      true,
		PGID:       pid,
		StartToken: fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime),
	}
}

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(id.PID))
	if err != nil {
		if isWindowsProcessMissing(err) {
			return os.ErrProcessDone
		}
		return err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return err
	}
	if code != 259 {
		return os.ErrProcessDone
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return err
	}
	info := processInfo{
		Alive:      true,
		PGID:       id.PID,
		StartToken: fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime),
	}
	if !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(id.PID)).Run(); err != nil {
		return windows.TerminateProcess(h, 1)
	}
	return nil
}

type windowsProcEntry struct {
	pid        int
	ppid       int
	startToken string
	startNs    int64
}

func parseWindowsStartToken(token string) (int64, bool) {
	lowStr, highStr, ok := strings.Cut(token, ":")
	if !ok {
		return 0, false
	}
	low, err1 := strconv.ParseUint(lowStr, 10, 32)
	high, err2 := strconv.ParseUint(highStr, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	ft := windows.Filetime{LowDateTime: uint32(low), HighDateTime: uint32(high)}
	return ft.Nanoseconds(), true
}

func snapshotWindowsProcesses() ([]windowsProcEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, err
	}
	var out []windowsProcEntry
	for {
		pid := int(pe.ProcessID)
		ppid := int(pe.ParentProcessID)
		if pid > 0 {
			info := inspectProcess(pid)
			if info.Alive {
				if ns, ok := parseWindowsStartToken(info.StartToken); ok {
					out = append(out, windowsProcEntry{
						pid:        pid,
						ppid:       ppid,
						startToken: info.StartToken,
						startNs:    ns,
					})
				}
			}
		}
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(snap, &pe); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return out, err
		}
	}
	return out, nil
}

func resolveWindowsGroupDescendants(proof *processGroupProof, id ProcessIdentityInfo, leaderNs int64, leaderAlive bool, procs []windowsProcEntry) ([]windowsProcEntry, error) {
	known := mergeGroupMembers(nil, proof.KnownMembers)
	verifiedMap := make(map[int]windowsProcEntry)
	for _, e := range procs {
		if e.pid == id.PID {
			if e.startToken != id.StartToken {
				return nil, fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
			}
			known[e.pid] = e.startToken
			verifiedMap[e.pid] = e
			continue
		}
		if e.startNs < leaderNs {
			continue
		}
		if known[e.pid] == e.startToken {
			verifiedMap[e.pid] = e
			continue
		}
		if e.ppid == id.PID {
			withinExitWindow := leaderAlive || (!proof.VerifiedAt.IsZero() && !proof.ExitObservedAt.IsZero() && e.startNs <= proof.ExitObservedAt.Add(2*time.Second).UnixNano())
			if withinExitWindow {
				known[e.pid] = e.startToken
				verifiedMap[e.pid] = e
			}
		}
	}
	for {
		added := false
		for _, e := range procs {
			if _, ok := verifiedMap[e.pid]; ok || e.startNs < leaderNs {
				continue
			}
			if parentTok, ok := known[e.ppid]; ok {
				parentNs, parsed := parseWindowsStartToken(parentTok)
				if parsed && e.startNs >= parentNs {
					known[e.pid] = e.startToken
					verifiedMap[e.pid] = e
					added = true
				}
			}
		}
		if !added {
			break
		}
	}
	proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, known)
	verified := make([]windowsProcEntry, 0, len(verifiedMap))
	for _, e := range verifiedMap {
		verified = append(verified, e)
	}
	return verified, nil
}

func terminateVerifiedWindowsProcess(pid int, expectedToken string) (signalled bool, err error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		if isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	if code != 259 {
		return false, nil
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		if isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	tok := fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime)
	if tok != expectedToken {
		return false, fmt.Errorf("refusing to signal pid %d: %w", pid, ErrProcessIdentityMismatch)
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		if isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func stopVerifiedProcessGroupWithProof(proof processGroupProof) error {
	return stopVerifiedProcessGroup(proof.Leader)
}

func captureVerifiedGroupMembers(proof processGroupProof) map[int]string {
	id := proof.Leader
	if !id.Valid() {
		return nil
	}
	leaderNs, ok := parseWindowsStartToken(id.StartToken)
	if !ok {
		return nil
	}
	info := inspectProcess(id.PID)
	if !id.Matches(info) {
		return nil
	}
	procs, err := snapshotWindowsProcesses()
	if err != nil {
		return map[int]string{id.PID: id.StartToken}
	}
	cloned := cloneGroupProof(proof)
	if _, rerr := resolveWindowsGroupDescendants(&cloned, id, leaderNs, true, procs); rerr != nil {
		return nil
	}
	return cloned.KnownMembers
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
}

func closeVerifiedGroupProof(proof *processGroupProof) {}

func stopAdoptedProcessGroupAfterExit(proof processGroupProof) error {
	cloned := cloneGroupProof(proof)
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
	leaderNs, ok := parseWindowsStartToken(id.StartToken)
	if !ok {
		return ErrProcessIdentityMismatch
	}
	info := inspectProcess(id.PID)
	if info.Alive && !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}

	const maxPasses = 16
	deadline := time.Now().Add(2 * time.Second)
	anySignalled := false
	consecutiveEmpty := 0
	for pass := 0; pass < maxPasses; pass++ {
		procs, err := snapshotWindowsProcesses()
		if err != nil {
			return err
		}
		leaderAlive := false
		for _, e := range procs {
			if e.pid == id.PID && e.startToken == id.StartToken {
				leaderAlive = true
				break
			}
		}
		verified, rerr := resolveWindowsGroupDescendants(proof, id, leaderNs, leaderAlive, procs)
		if rerr != nil {
			return rerr
		}
		if len(verified) == 0 {
			consecutiveEmpty++
			if consecutiveEmpty >= 2 {
				if anySignalled {
					return nil
				}
				return os.ErrProcessDone
			}
			time.Sleep(2 * time.Millisecond)
			continue
		}
		consecutiveEmpty = 0
		var termErr error
		passSignalled := false
		for _, cand := range verified {
			sig, serr := terminateVerifiedWindowsProcess(cand.pid, cand.startToken)
			if serr != nil {
				if errors.Is(serr, ErrProcessIdentityMismatch) {
					return serr
				}
				if termErr == nil {
					termErr = serr
				}
			} else if sig {
				passSignalled = true
				anySignalled = true
			}
		}
		if termErr != nil {
			return termErr
		}
		if pass == maxPasses-1 || time.Now().After(deadline) {
			break
		}
		if passSignalled {
			time.Sleep(10 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return fmt.Errorf("verified Windows process tree for pid %d still has live members: %w", id.PID, errGroupMembersStillAlive)
}

func hasVerifiedLiveGroupMember(proof processGroupProof) (bool, error) {
	id := proof.Leader
	if !id.Valid() {
		return false, ErrProcessIdentityMismatch
	}
	leaderNs, ok := parseWindowsStartToken(id.StartToken)
	if !ok {
		return false, ErrProcessIdentityMismatch
	}
	procs, err := snapshotWindowsProcesses()
	if err != nil {
		return false, err
	}
	cloned := cloneGroupProof(proof)
	verified, rerr := resolveWindowsGroupDescendants(&cloned, id, leaderNs, false, procs)
	if rerr != nil {
		return false, rerr
	}
	return len(verified) > 0, nil
}

func waitAdoptedProcess(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
	if !id.Valid() {
		return -1, false, ErrProcessIdentityMismatch
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(id.PID))
	if err != nil {
		if isWindowsProcessMissing(err) {
			return -1, false, errAdoptedProcessExited
		}
		return -1, false, err
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return -1, false, err
	}
	info := processInfo{
		Alive:      true,
		PGID:       id.PID,
		StartToken: fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime),
	}
	if !id.Matches(info) {
		return -1, false, ErrProcessIdentityMismatch
	}

	cancelEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return -1, false, err
	}
	defer windows.CloseHandle(cancelEvent)

	stopWake := context.AfterFunc(ctx, func() { _ = windows.SetEvent(cancelEvent) })
	defer stopWake()
	if ctx.Err() != nil {
		return -1, false, ctx.Err()
	}

	ev, err := windows.WaitForMultipleObjects([]windows.Handle{h, cancelEvent}, false, windows.INFINITE)
	if err != nil {
		return -1, false, err
	}
	if ev == uint32(windows.WAIT_OBJECT_0)+1 {
		return -1, false, ctx.Err()
	}
	if ev != uint32(windows.WAIT_OBJECT_0) {
		return -1, false, fmt.Errorf("unexpected process wait status %d", ev)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return -1, false, errAdoptedProcessExited
	}
	if code == 259 {
		return -1, false, errAdoptedProcessExited
	}
	if code != 0 {
		return int(code), true, fmt.Errorf("exit: exit status %d", code)
	}
	return 0, true, nil
}

func inspectProcessErr(pid int) (processInfo, error) {
	return inspectProcess(pid), nil
}
