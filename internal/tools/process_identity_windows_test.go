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
			{pid: 999, ppid: 4, inspectErr: windows.ERROR_PARTIAL_COPY},
			{pid: 201, ppid: 100, startToken: memberTok, startNs: memberNs},
		}
		verified, inconclusiveErr, err := resolveWindowsGroupDescendants(&proof, id, leaderNs, false, procs)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(verified) != 1 || verified[0].pid != 201 {
			t.Fatalf("verified = %+v, want proven member [201] even with speculative probe failure", verified)
		}
		if !errors.Is(inconclusiveErr, windows.ERROR_PARTIAL_COPY) {
			t.Fatalf("inconclusiveErr = %v, want ERROR_PARTIAL_COPY", inconclusiveErr)
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
