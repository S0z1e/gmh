//go:build windows

package tools

import (
	"errors"
	"fmt"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

func windowsTokenAt(t time.Time) (string, int64) {
	ft := windows.NsecToFiletime(t.UnixNano())
	return fmt.Sprintf("%d:%d", ft.LowDateTime, ft.HighDateTime), ft.Nanoseconds()
}

func TestResolveWindowsGroupDescendantsRejectsPIDReuseReplacementParent(t *testing.T) {
	base := time.Unix(1700000000, 0)
	leaderTok, leaderNs := windowsTokenAt(base)
	childTok, _ := windowsTokenAt(base.Add(1 * time.Second))
	liveSiblingTok, liveSiblingNs := windowsTokenAt(base.Add(2 * time.Second))
	replacementTok, replacementNs := windowsTokenAt(base.Add(10 * time.Second))
	unrelatedChildTok, unrelatedChildNs := windowsTokenAt(base.Add(11 * time.Second))

	id := ProcessIdentityInfo{PID: 100, PGID: 100, StartToken: leaderTok}
	proof := processGroupProof{
		Leader:     id,
		VerifiedAt: base.Add(3 * time.Second),
		KnownMembers: map[int]string{
			100: leaderTok,
			200: childTok,
			201: liveSiblingTok,
		},
	}

	// PID 200 (previous child) exited and was reused by an unrelated replacement
	// process that spawned PID 300. PID 201 is still a genuine live group member.
	procs := []windowsProcEntry{
		{pid: 200, ppid: 50, startToken: replacementTok, startNs: replacementNs},
		{pid: 300, ppid: 200, startToken: unrelatedChildTok, startNs: unrelatedChildNs},
		{pid: 201, ppid: 100, startToken: liveSiblingTok, startNs: liveSiblingNs},
	}

	verified, inconclusiveErr, err := resolveWindowsGroupDescendants(&proof, id, leaderNs, false, procs)
	if err != nil || inconclusiveErr != nil {
		t.Fatalf("resolveWindowsGroupDescendants err=%v inconclusive=%v", err, inconclusiveErr)
	}
	if len(verified) != 1 || verified[0].pid != 201 {
		t.Fatalf("verified = %+v, want only genuine live member [201]", verified)
	}
	if proof.KnownMembers[200] != childTok {
		t.Fatalf("KnownMembers[200] overwritten with replacement token %q", proof.KnownMembers[200])
	}
	if _, ok := proof.KnownMembers[300]; ok {
		t.Fatalf("unrelated replacement child 300 added to KnownMembers: %+v", proof.KnownMembers)
	}
}

func TestResolveWindowsGroupDescendantsTracksSurvivingChildAfterIntermediateExits(t *testing.T) {
	base := time.Unix(1700000100, 0)
	leaderTok, leaderNs := windowsTokenAt(base)
	grandTok, grandNs := windowsTokenAt(base.Add(4 * time.Second))
	grandTok2, grandNs2 := windowsTokenAt(base.Add(5 * time.Second))

	for _, tc := range []struct {
		name     string
		entry    windowsProcEntry
		jobToken string
	}{
		{
			name:     "job_token",
			jobToken: "tok-win-grandchild",
			entry: windowsProcEntry{
				pid:        300,
				ppid:       200, // unobserved intermediate 200 already exited
				startToken: grandTok,
				startNs:    grandNs,
				hasJobTok:  true,
			},
		},
		{
			name: "job_object",
			entry: windowsProcEntry{
				pid:        300,
				ppid:       200, // unobserved intermediate 200 already exited
				startToken: grandTok2,
				startNs:    grandNs2,
				inJobObj:   true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := ProcessIdentityInfo{PID: 100, PGID: 100, StartToken: leaderTok}
			proof := processGroupProof{
				Leader:       id,
				JobToken:     tc.jobToken,
				VerifiedAt:   base.Add(1 * time.Second),
				KnownMembers: map[int]string{100: leaderTok},
			}
			procs := []windowsProcEntry{tc.entry}
			verified, inconclusiveErr, err := resolveWindowsGroupDescendants(&proof, id, leaderNs, false, procs)
			if err != nil || inconclusiveErr != nil {
				t.Fatalf("resolveWindowsGroupDescendants err=%v inconclusive=%v", err, inconclusiveErr)
			}
			if len(verified) != 1 || verified[0].pid != 300 {
				t.Fatalf("verified = %+v, want [300]", verified)
			}
		})
	}
}

func TestResolveWindowsGroupDescendantsPreservesInspectionErrors(t *testing.T) {
	base := time.Unix(1700000200, 0)
	leaderTok, leaderNs := windowsTokenAt(base)
	memberTok, memberNs := windowsTokenAt(base.Add(2 * time.Second))
	id := ProcessIdentityInfo{PID: 100, PGID: 100, StartToken: leaderTok}

	t.Run("descendant_access_denied", func(t *testing.T) {
		proof := processGroupProof{Leader: id, KnownMembers: map[int]string{100: leaderTok}}
		procs := []windowsProcEntry{
			{pid: 250, ppid: 100, inspectErr: windows.ERROR_ACCESS_DENIED},
		}
		verified, inconclusiveErr, err := resolveWindowsGroupDescendants(&proof, id, leaderNs, false, procs)
		if err != nil || len(verified) != 0 || !errors.Is(inconclusiveErr, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("got verified=%+v inconclusive=%v err=%v, want inconclusive=ERROR_ACCESS_DENIED", verified, inconclusiveErr, err)
		}
	})

	t.Run("speculative_probe_failure_does_not_block_verified_members", func(t *testing.T) {
		proof := processGroupProof{
			Leader:       id,
			KnownMembers: map[int]string{100: leaderTok, 201: memberTok},
		}
		procs := []windowsProcEntry{
			{pid: 999, ppid: 4, tokenProbeErr: windows.ERROR_PARTIAL_COPY, startNs: memberNs},
			{pid: 201, ppid: 100, startToken: memberTok, startNs: memberNs, tokenProbeErr: windows.ERROR_PARTIAL_COPY},
		}
		verified, inconclusiveErr, err := resolveWindowsGroupDescendants(&proof, id, leaderNs, false, procs)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(verified) != 1 || verified[0].pid != 201 {
			t.Fatalf("verified = %+v, want proven member [201] even with failing PEB probe on it", verified)
		}
		if !errors.Is(inconclusiveErr, windows.ERROR_PARTIAL_COPY) {
			t.Fatalf("inconclusiveErr = %v, want ERROR_PARTIAL_COPY for uncertain pid 999", inconclusiveErr)
		}
	})

	t.Run("known_member_token_match_clears_optional_probe_error", func(t *testing.T) {
		proof := processGroupProof{
			Leader:       id,
			KnownMembers: map[int]string{100: leaderTok, 201: memberTok},
		}
		procs := []windowsProcEntry{
			{pid: 201, ppid: 100, startToken: memberTok, startNs: memberNs, tokenProbeErr: windows.ERROR_PARTIAL_COPY},
		}
		verified, inconclusiveErr, err := resolveWindowsGroupDescendants(&proof, id, leaderNs, false, procs)
		if err != nil || inconclusiveErr != nil {
			t.Fatalf("err=%v inconclusive=%v, want both nil", err, inconclusiveErr)
		}
		if len(verified) != 1 || verified[0].pid != 201 {
			t.Fatalf("verified = %+v, want [201]", verified)
		}
	})

	t.Run("unrelated_system_access_denied_ignored", func(t *testing.T) {
		proof := processGroupProof{Leader: id, KnownMembers: map[int]string{100: leaderTok}}
		procs := []windowsProcEntry{
			{pid: 4, ppid: 0, inspectErr: windows.ERROR_ACCESS_DENIED},
		}
		verified, inconclusiveErr, err := resolveWindowsGroupDescendants(&proof, id, leaderNs, false, procs)
		if err != nil || inconclusiveErr != nil {
			t.Fatalf("inconclusive=%v err=%v, want nil", inconclusiveErr, err)
		}
		if len(verified) != 0 {
			t.Fatalf("verified = %+v, want empty", verified)
		}
	})
}

func TestParseWindowsEnvHasToken(t *testing.T) {
	target := utf16.Encode([]rune(jobTokenEnvVar + "=secret-123"))
	envBlock := utf16.Encode([]rune(
		"PATH=C:\\Windows\\system32\x00" +
			jobTokenEnvVar + "=secret-1234\x00" +
			jobTokenEnvVar + "=secret-123\x00" +
			"\x00",
	))
	if !parseWindowsEnvHasToken(envBlock, target) {
		t.Fatal("expected exact env token match")
	}
	wrongBlock := utf16.Encode([]rune(
		jobTokenEnvVar + "=secret-1234\x00" +
			"OTHER=value\x00" +
			"\x00" +
			jobTokenEnvVar + "=secret-123\x00", // after double-NUL terminator
	))
	if parseWindowsEnvHasToken(wrongBlock, target) {
		t.Fatal("expected false for wrong value and entry after double-NUL")
	}
}

func TestStopAdoptedWindowsGroupTreatsRecycledChildExitAsGoneAndStopsSiblings(t *testing.T) {
	base := time.Unix(1700000300, 0)
	leaderTok, _ := windowsTokenAt(base)
	child1Tok, child1Ns := windowsTokenAt(base.Add(1 * time.Second))
	child2Tok, child2Ns := windowsTokenAt(base.Add(2 * time.Second))
	replacementTok, replacementNs := windowsTokenAt(base.Add(10 * time.Second))

	id := ProcessIdentityInfo{PID: 100, PGID: 100, StartToken: leaderTok}
	proof := processGroupProof{
		Leader:     id,
		VerifiedAt: base.Add(3 * time.Second),
		KnownMembers: map[int]string{
			100: leaderTok,
			201: child1Tok,
			202: child2Tok,
		},
	}

	origInspect := inspectWindowsProcessFn
	origSnap := snapshotWindowsProcessesFn
	origTerm := terminateWindowsProcessFn
	defer func() {
		inspectWindowsProcessFn = origInspect
		snapshotWindowsProcessesFn = origSnap
		terminateWindowsProcessFn = origTerm
	}()

	inspectWindowsProcessFn = func(pid int) (processInfo, error) {
		return processInfo{}, nil
	}

	var child2Signalled bool
	pass := 0
	snapshotWindowsProcessesFn = func(ln int64, jobTok string, jobHandle uintptr) ([]windowsProcEntry, error) {
		pass++
		if pass == 1 {
			return []windowsProcEntry{
				{pid: 201, ppid: 100, startToken: child1Tok, startNs: child1Ns},
				{pid: 202, ppid: 100, startToken: child2Tok, startNs: child2Ns},
			}, nil
		}
		// After pass 1, child 201 was replaced at its recycled PID and child 202 exited.
		return []windowsProcEntry{
			{pid: 201, ppid: 50, startToken: replacementTok, startNs: replacementNs},
		}, nil
	}
	terminateWindowsProcessFn = func(cand windowsProcEntry, expectedToken string, jobHandle uintptr) (bool, error) {
		if cand.pid == 201 {
			// Child 201 exited and its PID was recycled between snapshot and OpenProcess.
			return false, fmt.Errorf("refusing to signal pid %d: %w", cand.pid, ErrProcessIdentityMismatch)
		}
		if cand.pid == 202 {
			child2Signalled = true
			return true, nil
		}
		return false, nil
	}

	if err := stopAdoptedProcessGroupAfterExitTrack(&proof); err != nil {
		t.Fatalf("stopAdoptedProcessGroupAfterExitTrack err = %v, want nil (siblings still stopped)", err)
	}
	if !child2Signalled {
		t.Fatal("expected sibling pid 202 to be signalled after pid 201 recycled")
	}
}

func TestStopAdoptedWindowsGroupContinuesAfterLeaderPIDRecycled(t *testing.T) {
	base := time.Unix(1700000400, 0)
	leaderTok, _ := windowsTokenAt(base)
	childTok, childNs := windowsTokenAt(base.Add(1 * time.Second))
	replacementTok, replacementNs := windowsTokenAt(base.Add(10 * time.Second))

	id := ProcessIdentityInfo{PID: 100, PGID: 100, StartToken: leaderTok}
	proof := processGroupProof{
		Leader:         id,
		VerifiedAt:     base.Add(2 * time.Second),
		ExitObservedAt: base.Add(5 * time.Second),
		KnownMembers: map[int]string{
			100: leaderTok,
			201: childTok,
		},
	}

	origInspect := inspectWindowsProcessFn
	origSnap := snapshotWindowsProcessesFn
	origTerm := terminateWindowsProcessFn
	defer func() {
		inspectWindowsProcessFn = origInspect
		snapshotWindowsProcessesFn = origSnap
		terminateWindowsProcessFn = origTerm
	}()

	// Case 1: replacement process already occupies id.PID at pre-scan time AND during snapshot.
	inspectWindowsProcessFn = func(pid int) (processInfo, error) {
		if pid == 100 {
			return processInfo{Alive: true, PGID: 100, StartToken: replacementTok}, nil
		}
		return processInfo{}, nil
	}

	var childSignalled, replacementSignalled bool
	pass := 0
	snapshotWindowsProcessesFn = func(ln int64, jobTok string, jobHandle uintptr) ([]windowsProcEntry, error) {
		pass++
		if pass == 1 {
			return []windowsProcEntry{
				{pid: 100, ppid: 50, startToken: replacementTok, startNs: replacementNs},
				{pid: 300, ppid: 100, startToken: replacementTok, startNs: replacementNs},
				{pid: 201, ppid: 100, startToken: childTok, startNs: childNs},
			}, nil
		}
		return []windowsProcEntry{
			{pid: 100, ppid: 50, startToken: replacementTok, startNs: replacementNs},
			{pid: 300, ppid: 100, startToken: replacementTok, startNs: replacementNs},
		}, nil
	}
	terminateWindowsProcessFn = func(cand windowsProcEntry, expectedToken string, jobHandle uintptr) (bool, error) {
		if cand.pid == 100 || cand.pid == 300 {
			replacementSignalled = true
			return false, fmt.Errorf("unexpectedly targeted pid %d", cand.pid)
		}
		if cand.pid == 201 {
			childSignalled = true
			return true, nil
		}
		return false, nil
	}

	if err := stopAdoptedProcessGroupAfterExitTrack(&proof); err != nil {
		t.Fatalf("stopAdoptedProcessGroupAfterExitTrack err = %v, want nil", err)
	}
	if !childSignalled {
		t.Fatal("expected verified surviving child 201 to be signalled despite leader PID reuse")
	}
	if replacementSignalled {
		t.Fatal("replacement process at leader PID or its child must never be signalled")
	}
}
