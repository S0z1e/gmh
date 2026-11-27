//go:build windows

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var inspectWindowsProcessFn = inspectProcessErr

func isWindowsProcessMissing(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

func processGroupID(pid int) int {
	if pid <= 0 {
		return 0
	}
	return pid
}

func openVerifiedWindowsProcess(pid int, expectedToken string, desiredAccess uint32) (windows.Handle, error) {
	if pid <= 0 || expectedToken == "" {
		return 0, ErrProcessIdentityMismatch
	}
	h, err := windows.OpenProcess(desiredAccess, false, uint32(pid))
	if err != nil {
		if isWindowsProcessMissing(err) {
			return 0, os.ErrProcessDone
		}
		return 0, err
	}
	closeHandle := true
	defer func() {
		if closeHandle {
			_ = windows.CloseHandle(h)
		}
	}()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		if isWindowsProcessMissing(err) {
			return 0, os.ErrProcessDone
		}
		return 0, err
	}
	if code != 259 { // STILL_ACTIVE
		return 0, os.ErrProcessDone
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		if isWindowsProcessMissing(err) {
			return 0, os.ErrProcessDone
		}
		return 0, err
	}
	tok := fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime)
	if tok != expectedToken {
		return 0, fmt.Errorf("refusing to signal pid %d: %w", pid, ErrProcessIdentityMismatch)
	}
	closeHandle = false
	return h, nil
}

func inspectProcess(pid int) processInfo {
	info, _ := inspectProcessErr(pid)
	return info
}

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	if !id.Valid() {
		return ErrProcessIdentityMismatch
	}
	h, err := openVerifiedWindowsProcess(id.PID, id.StartToken, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
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
	hasJobTok  bool
	inJobObj   bool
	inspectErr error
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

func parseWindowsEnvHasToken(u16 []uint16, target []uint16) bool {
	if len(target) == 0 || len(u16) < len(target) {
		return false
	}
	start := 0
	for i := 0; i < len(u16); i++ {
		if u16[i] != 0 {
			continue
		}
		if i == start {
			break
		}
		if slices.Equal(u16[start:i], target) {
			return true
		}
		start = i + 1
	}
	return false
}

func windowsEnvBlockDoubleNulEnded(u16 []uint16) bool {
	for i := 0; i+1 < len(u16); i++ {
		if u16[i] == 0 && u16[i+1] == 0 {
			return true
		}
	}
	return false
}

func processHasJobTokenWindowsErr(pid int, token string) (bool, error) {
	if pid <= 0 || token == "" {
		return false, nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		if isWindowsProcessMissing(err) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
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

	var pbi windows.PROCESS_BASIC_INFORMATION
	if err := windows.NtQueryInformationProcess(h, 0, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), nil); err != nil {
		if codeErr := windows.GetExitCodeProcess(h, &code); codeErr == nil && code != 259 {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	if pbi.PebBaseAddress == nil {
		return false, nil
	}

	var peb windows.PEB
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(pbi.PebBaseAddress)), (*byte)(unsafe.Pointer(&peb)), unsafe.Sizeof(peb), nil); err != nil {
		if codeErr := windows.GetExitCodeProcess(h, &code); codeErr == nil && code != 259 {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	if peb.ProcessParameters == nil {
		return false, nil
	}

	var params windows.RTL_USER_PROCESS_PARAMETERS
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(peb.ProcessParameters)), (*byte)(unsafe.Pointer(&params)), unsafe.Sizeof(params), nil); err != nil {
		if codeErr := windows.GetExitCodeProcess(h, &code); codeErr == nil && code != 259 {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	if params.Environment == nil {
		return false, nil
	}

	target := utf16.Encode([]rune(jobTokenEnvVar + "=" + token))
	const maxEnvBytes = 1 << 20
	if params.EnvironmentSize > 0 {
		readBytes := int(min(params.EnvironmentSize, maxEnvBytes))
		words := readBytes / 2
		if words == 0 {
			return false, nil
		}
		buf := make([]uint16, words)
		var bytesRead uintptr
		if err := windows.ReadProcessMemory(h, uintptr(params.Environment), (*byte)(unsafe.Pointer(&buf[0])), uintptr(words*2), &bytesRead); err != nil {
			if codeErr := windows.GetExitCodeProcess(h, &code); codeErr == nil && code != 259 {
				return false, nil
			}
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) || isWindowsProcessMissing(err) {
				return false, nil
			}
			return false, err
		}
		return parseWindowsEnvHasToken(buf[:bytesRead/2], target), nil
	}

	const chunkBytes = 4096
	var combined []uint16
	base := uintptr(params.Environment)
	for offset := uintptr(0); offset < maxEnvBytes; offset += chunkBytes {
		chunk := make([]uint16, chunkBytes/2)
		var bytesRead uintptr
		err := windows.ReadProcessMemory(h, base+offset, (*byte)(unsafe.Pointer(&chunk[0])), uintptr(chunkBytes), &bytesRead)
		if bytesRead >= 2 {
			combined = append(combined, chunk[:bytesRead/2]...)
			if parseWindowsEnvHasToken(combined, target) {
				return true, nil
			}
			if windowsEnvBlockDoubleNulEnded(combined) {
				return false, nil
			}
		}
		if err != nil {
			if codeErr := windows.GetExitCodeProcess(h, &code); codeErr == nil && code != 259 {
				return false, nil
			}
			if errors.Is(err, windows.ERROR_PARTIAL_COPY) && len(combined) > 0 {
				return parseWindowsEnvHasToken(combined, target), nil
			}
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) || isWindowsProcessMissing(err) {
				return false, nil
			}
			return false, err
		}
	}
	return parseWindowsEnvHasToken(combined, target), nil
}

type jobObjectBasicProcessIdListHeader struct {
	numberOfAssignedProcesses uint32
	numberOfProcessIdsInList  uint32
}

func queryWindowsJobObjectPIDs(job windows.Handle) (map[int]bool, error) {
	if job == 0 {
		return nil, nil
	}
	headerSize := unsafe.Sizeof(jobObjectBasicProcessIdListHeader{})
	ptrSize := unsafe.Sizeof(uintptr(0))
	capPIDs := 64
	for {
		raw := make([]byte, int(headerSize)+capPIDs*int(ptrSize))
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&raw[0])), uint32(len(raw)), nil)
		hdr := (*jobObjectBasicProcessIdListHeader)(unsafe.Pointer(&raw[0]))
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			nextCap := int(hdr.numberOfAssignedProcesses) + 16
			if nextCap <= capPIDs {
				nextCap = capPIDs * 2
			}
			capPIDs = nextCap
			continue
		}
		if err != nil {
			return nil, err
		}
		pids := make(map[int]bool, int(hdr.numberOfProcessIdsInList))
		for i := 0; i < int(hdr.numberOfProcessIdsInList); i++ {
			offset := int(headerSize) + i*int(ptrSize)
			if offset+int(ptrSize) > len(raw) {
				break
			}
			pid := int(*(*uintptr)(unsafe.Pointer(&raw[offset])))
			if pid > 0 {
				pids[pid] = true
			}
		}
		return pids, nil
	}
}

func assignVerifiedMembersToJobObject(job windows.Handle, members map[int]string) {
	if job == 0 || len(members) == 0 {
		return
	}
	for pid, tok := range members {
		h, err := openVerifiedWindowsProcess(pid, tok, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE)
		if err != nil {
			continue
		}
		_ = windows.AssignProcessToJobObject(job, h)
		_ = windows.CloseHandle(h)
	}
}

func snapshotWindowsProcesses(leaderNs int64, jobToken string, jobHandle uintptr) ([]windowsProcEntry, error) {
	var jobPIDs map[int]bool
	if jobHandle != 0 {
		var jerr error
		jobPIDs, jerr = queryWindowsJobObjectPIDs(windows.Handle(jobHandle))
		if jerr != nil {
			return nil, jerr
		}
	}
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
			info, ierr := inspectWindowsProcessFn(pid)
			if ierr != nil {
				out = append(out, windowsProcEntry{
					pid:        pid,
					ppid:       ppid,
					inJobObj:   jobPIDs[pid],
					inspectErr: ierr,
				})
			} else if info.Alive {
				if ns, ok := parseWindowsStartToken(info.StartToken); ok {
					entry := windowsProcEntry{
						pid:        pid,
						ppid:       ppid,
						startToken: info.StartToken,
						startNs:    ns,
						inJobObj:   jobPIDs[pid],
					}
					if jobToken != "" && leaderNs > 0 && ns >= leaderNs && !entry.inJobObj {
						hasTok, tokErr := processHasJobTokenWindowsErr(pid, jobToken)
						if tokErr != nil {
							entry.inspectErr = tokErr
						} else {
							entry.hasJobTok = hasTok
						}
					}
					out = append(out, entry)
				}
			}
		}
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(snap, &pe); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, err
		}
	}
	return out, nil
}

func resolveWindowsGroupDescendants(proof *processGroupProof, id ProcessIdentityInfo, leaderNs int64, _ bool, procs []windowsProcEntry) ([]windowsProcEntry, error) {
	verifiedMap := make(map[int]windowsProcEntry)
	reusedKnown := make(map[int]bool)

	for _, e := range procs {
		if e.pid == id.PID {
			if e.inspectErr != nil {
				return nil, e.inspectErr
			}
			if e.startToken != id.StartToken {
				return nil, fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
			}
			verifiedMap[e.pid] = e
			continue
		}
		if prevTok, isKnown := proof.KnownMembers[e.pid]; isKnown {
			if e.inspectErr != nil {
				return nil, e.inspectErr
			}
			if e.startToken == prevTok {
				verifiedMap[e.pid] = e
				continue
			}
			if e.startNs >= leaderNs && (e.hasJobTok || e.inJobObj) {
				verifiedMap[e.pid] = e
				continue
			}
			// PID was reused by an unrelated replacement process. Never select it
			// and never treat it as the original parent instance for ancestry.
			reusedKnown[e.pid] = true
		}
	}

	for _, e := range procs {
		if e.inspectErr != nil {
			_, knownParent := proof.KnownMembers[e.ppid]
			if !errors.Is(e.inspectErr, windows.ERROR_ACCESS_DENIED) || e.ppid == id.PID || knownParent || e.inJobObj {
				return nil, e.inspectErr
			}
			continue
		}
		if _, ok := verifiedMap[e.pid]; ok || e.startNs < leaderNs {
			continue
		}
		if e.hasJobTok || e.inJobObj {
			verifiedMap[e.pid] = e
		}
	}

	for {
		added := false
		for _, e := range procs {
			if e.inspectErr != nil {
				if _, ok := verifiedMap[e.ppid]; ok {
					return nil, e.inspectErr
				}
				continue
			}
			if _, ok := verifiedMap[e.pid]; ok || e.startNs < leaderNs {
				continue
			}
			// Parent PID alone cannot distinguish an original exited/replaced parent
			// from PID reuse, so require the exact original parent instance to be
			// verified live in this snapshot.
			if parent, ok := verifiedMap[e.ppid]; ok && !reusedKnown[e.ppid] && e.startNs >= parent.startNs {
				verifiedMap[e.pid] = e
				added = true
			}
		}
		if !added {
			break
		}
	}

	if proof.KnownMembers == nil && len(verifiedMap) > 0 {
		proof.KnownMembers = make(map[int]string, len(verifiedMap))
	}
	for pid, e := range verifiedMap {
		if _, reused := reusedKnown[pid]; !reused {
			proof.KnownMembers[pid] = e.startToken
		}
	}
	verified := make([]windowsProcEntry, 0, len(verifiedMap))
	for _, e := range verifiedMap {
		verified = append(verified, e)
	}
	return verified, nil
}

func terminateVerifiedWindowsProcess(pid int, expectedToken string) (signalled bool, err error) {
	h, err := openVerifiedWindowsProcess(pid, expectedToken, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE)
	if err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		if isWindowsProcessMissing(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func stopVerifiedProcessGroupWithProof(proof processGroupProof) error {
	err := stopVerifiedProcessGroup(proof.Leader)
	if (err == nil || errors.Is(err, os.ErrProcessDone)) && proof.jobHandle != 0 {
		_ = windows.TerminateJobObject(windows.Handle(proof.jobHandle), 1)
	}
	return err
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
	info, ierr := inspectWindowsProcessFn(id.PID)
	if ierr != nil || !id.Matches(info) {
		return nil
	}
	procs, err := snapshotWindowsProcesses(leaderNs, proof.JobToken, proof.jobHandle)
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
	id := proof.Leader
	if !id.Valid() {
		return
	}
	info, ierr := inspectWindowsProcessFn(id.PID)
	if ierr != nil || !id.Matches(info) {
		return
	}
	if proof.jobHandle == 0 {
		if job, err := windows.CreateJobObject(nil, nil); err == nil {
			proof.jobHandle = uintptr(job)
		}
	}
	members := captureVerifiedGroupMembers(*proof)
	if len(members) == 0 {
		members = map[int]string{id.PID: id.StartToken}
	}
	proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, members)
	assignVerifiedMembersToJobObject(windows.Handle(proof.jobHandle), proof.KnownMembers)
	if proof.KnownMembers[proof.Leader.PID] == proof.Leader.StartToken {
		proof.VerifiedAt = time.Now()
	}
}

func closeVerifiedGroupProof(proof *processGroupProof) {
	if proof == nil {
		return
	}
	if proof.jobHandle != 0 {
		_ = windows.CloseHandle(windows.Handle(proof.jobHandle))
		proof.jobHandle = 0
	}
}

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
	info, ierr := inspectWindowsProcessFn(id.PID)
	if ierr != nil {
		return ierr
	}
	if info.Alive && !id.Matches(info) {
		return fmt.Errorf("refusing to signal pid %d: %w", id.PID, ErrProcessIdentityMismatch)
	}

	const maxPasses = 16
	deadline := time.Now().Add(2 * time.Second)
	anySignalled := false
	consecutiveEmpty := 0
	for pass := 0; pass < maxPasses; pass++ {
		procs, err := snapshotWindowsProcesses(leaderNs, proof.JobToken, proof.jobHandle)
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
	procs, err := snapshotWindowsProcesses(leaderNs, proof.JobToken, proof.jobHandle)
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
	if pid <= 0 {
		return processInfo{}, nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if isWindowsProcessMissing(err) {
			return processInfo{}, nil
		}
		return processInfo{}, err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		if isWindowsProcessMissing(err) {
			return processInfo{}, nil
		}
		return processInfo{}, err
	}
	if code != 259 { // STILL_ACTIVE
		return processInfo{}, nil
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		if isWindowsProcessMissing(err) {
			return processInfo{}, nil
		}
		return processInfo{}, err
	}
	return processInfo{
		Alive:      true,
		PGID:       pid,
		StartToken: fmt.Sprintf("%d:%d", creation.LowDateTime, creation.HighDateTime),
	}, nil
}
