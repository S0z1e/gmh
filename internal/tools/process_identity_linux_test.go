//go:build linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func exhaustFDsUntilEMFILE(t *testing.T) func() {
	return exhaustFDsLeaving(t, 0)
}

func exhaustFDsLeaving(t *testing.T, leave int) func() {
	t.Helper()
	var openFDs []*os.File
	for range 4096 {
		f, err := os.Open("/dev/null")
		if err != nil {
			if errors.Is(err, syscall.EMFILE) {
				for i := 0; i < leave && len(openFDs) > 0; i++ {
					_ = openFDs[len(openFDs)-1].Close()
					openFDs = openFDs[:len(openFDs)-1]
				}
				return func() {
					for _, of := range openFDs {
						_ = of.Close()
					}
				}
			}
			for _, of := range openFDs {
				_ = of.Close()
			}
			t.Fatalf("unexpected open error before EMFILE: %v", err)
		}
		openFDs = append(openFDs, f)
	}
	for _, of := range openFDs {
		_ = of.Close()
	}
	t.Skip("RLIMIT_NOFILE too large to hit EMFILE within test cap")
	return func() {}
}

func TestStopVerifiedProcessGroupUnderEMFILELinux(t *testing.T) {
	var oldLim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &oldLim); err != nil {
		t.Fatal(err)
	}
	curMax := oldLim.Cur
	if curMax > 256 {
		newLim := oldLim
		newLim.Cur = 256
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &newLim); err == nil {
			defer func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &oldLim) }()
		}
	}

	cmd := exec.CommandContext(t.Context(), "bash", "-c", "exec sleep 30")
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() { _ = killProcessTree(cmd) })

	pid := cmd.Process.Pid
	info, err := inspectProcessErr(pid)
	if err != nil || !info.Alive {
		t.Fatalf("inspectProcessErr(%d) = %+v, err=%v", pid, info, err)
	}
	proof := processGroupProof{
		Leader: ProcessIdentityInfo{PID: pid, PGID: info.PGID, StartToken: info.StartToken},
		UID:    info.UID,
		HasUID: info.HasUID,
		SID:    info.SID,
	}
	captureVerifiedGroupProof(&proof)
	defer closeVerifiedGroupProof(&proof)

	cleanupFDs := exhaustFDsUntilEMFILE(t)
	_, _, waitErr := waitAdoptedProcess(context.Background(), proof.Leader)
	if !errors.Is(waitErr, syscall.EMFILE) {
		cleanupFDs()
		t.Fatalf("waitAdoptedProcess under EMFILE err = %v, want EMFILE", waitErr)
	}

	// Same PID, PGID, SID, and UID, but stale leader StartToken must be rejected.
	staleLeader := proof
	staleLeader.Leader.StartToken = "stale-boot:1"
	if mismatchErr := stopVerifiedProcessGroupWithProof(staleLeader); !errors.Is(mismatchErr, ErrProcessIdentityMismatch) {
		cleanupFDs()
		t.Fatalf("stale leader token under EMFILE err = %v, want ErrProcessIdentityMismatch", mismatchErr)
	}

	// Same PID, PGID, SID, and UID, but stale KnownMembers token in post-exit fallback must be rejected.
	staleMember := proof
	staleMember.KnownMembers = map[int]string{pid: "stale-boot:1"}
	if mismatchErr := stopAdoptedProcessGroupAfterExit(staleMember); !errors.Is(mismatchErr, ErrProcessIdentityMismatch) {
		cleanupFDs()
		t.Fatalf("stale KnownMembers token under EMFILE err = %v, want ErrProcessIdentityMismatch", mismatchErr)
	}

	// Unbacked proof with no verified FD must fail with EMFILE rather than signalling blindly.
	unbacked := processGroupProof{Leader: proof.Leader, UID: proof.UID, HasUID: proof.HasUID, SID: proof.SID}
	if unbackedErr := stopVerifiedProcessGroupWithProof(unbacked); !isFDLimitErr(unbackedErr) {
		cleanupFDs()
		t.Fatalf("unbacked stop under EMFILE err = %v, want EMFILE/ENFILE", unbackedErr)
	}

	wrongProof := proof
	wrongProof.Leader.PGID = info.PGID + 1
	if mismatchErr := stopVerifiedProcessGroupWithProof(wrongProof); !errors.Is(mismatchErr, ErrProcessIdentityMismatch) {
		cleanupFDs()
		t.Fatalf("stopVerifiedProcessGroupWithProof mismatched PGID under EMFILE err = %v, want ErrProcessIdentityMismatch", mismatchErr)
	}

	stopErr := stopVerifiedProcessGroupWithProof(proof)
	cleanupFDs()
	if stopErr != nil {
		t.Fatalf("stopVerifiedProcessGroupWithProof under EMFILE failed: %v", stopErr)
	}
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("process survived stopVerifiedProcessGroupWithProof under EMFILE")
	}

	// Post-exit EMFILE fallback must preserve EMFILE rather than returning
	// os.ErrProcessDone when captured members have exited and /proc cannot be read
	// to rule out later-forked descendants occupying the PGID.
	cleanupFDsAfterExit := exhaustFDsUntilEMFILE(t)
	postExitErr := stopAdoptedProcessGroupAfterExit(proof)
	cleanupFDsAfterExit()
	if !isFDLimitErr(postExitErr) {
		t.Fatalf("stopAdoptedProcessGroupAfterExit after leader exit under EMFILE err = %v, want EMFILE/ENFILE", postExitErr)
	}
}

func TestStopAdoptedProcessGroupAfterExitRefusesEqualIDReusedSessionLinux(t *testing.T) {
	root := t.TempDir()
	childPIDFile := filepath.Join(root, "child.pid")
	cmd := exec.CommandContext(t.Context(), "bash", "-c", fmt.Sprintf("sleep 30 & echo $! > %q", childPIDFile))
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	leaderPID := cmd.Process.Pid
	_ = cmd.Wait()

	pidBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child pid %q: %v", string(pidBytes), err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})

	childInfo := inspectProcess(childPID)
	if !childInfo.Alive || childInfo.PGID <= 0 {
		t.Fatalf("child pid %d not inspectable: %+v", childPID, childInfo)
	}

	// Simulates Linux ID recycling where a replacement session has equal PID, PGID,
	// and SID (due to Setsid: true), the replacement leader exited, and an old
	// once-verified proof from an earlier job whose numeric IDs match is checked.
	// Because the surviving child has no matching token/ancestry and lacks JobToken,
	// Linux stopAdoptedProcessGroupAfterExit MUST refuse to signal the child.
	childBoot, _, _ := parseLinuxStartToken(childInfo.StartToken)
	olderTok := "1"
	if childBoot != "" {
		olderTok = childBoot + ":1"
	}
	staleEqualProof := processGroupProof{
		Leader: ProcessIdentityInfo{
			PID:        leaderPID,
			PGID:       leaderPID,
			StartToken: olderTok,
		},
		UID:            childInfo.UID,
		HasUID:         childInfo.HasUID,
		SID:            leaderPID,
		KnownMembers:   map[int]string{leaderPID: olderTok},
		VerifiedAt:     time.Now().Add(-time.Minute),
		JobToken:       "stale-secret-token-not-matching-child",
		ExitObservedAt: time.Now(),
	}
	if err := stopAdoptedProcessGroupAfterExit(staleEqualProof); !errors.Is(err, ErrProcessIdentityMismatch) {
		t.Fatalf("equal-ID recycled session proof err = %v, want ErrProcessIdentityMismatch", err)
	}
	if !inspectProcess(childPID).Alive {
		t.Fatalf("child pid %d was killed by stale equal-ID proof", childPID)
	}
}

func TestStopAdoptedProcessGroupAfterExitEMFILEBoundsAndPartialCleanupLinux(t *testing.T) {
	var oldLim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &oldLim); err != nil {
		t.Fatal(err)
	}
	if oldLim.Cur > 256 {
		newLim := oldLim
		newLim.Cur = 256
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &newLim); err == nil {
			defer func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &oldLim) }()
		}
	}

	root := t.TempDir()
	c1File := filepath.Join(root, "c1.pid")
	c2File := filepath.Join(root, "c2.pid")
	releaseFile := filepath.Join(root, "release")
	jobTok := "test-emfile-job-token"
	cmd := exec.CommandContext(t.Context(), "bash", "-c",
		fmt.Sprintf("sleep 30 & echo $! > %q; while [ ! -f %q ]; do sleep 0.02; done; sleep 30 & echo $! > %q", c1File, releaseFile, c2File),
	)
	cmd.Env = append(os.Environ(), jobTokenEnvVar+"="+jobTok)
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	leaderPID := cmd.Process.Pid
	leaderInfo := inspectProcess(leaderPID)
	if !leaderInfo.Alive {
		t.Fatalf("leader pid %d not inspectable", leaderPID)
	}

	var c1PID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(c1File); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && p > 0 {
				c1PID = p
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if c1PID <= 0 {
		t.Fatal("c1 did not start")
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(c1PID); err == nil {
			_ = p.Kill()
		}
	})

	// Capture proof while only leader and c1 exist (c1 has a cached pidfd).
	proof := processGroupProof{
		Leader:   ProcessIdentityInfo{PID: leaderPID, PGID: leaderInfo.PGID, StartToken: leaderInfo.StartToken},
		UID:      leaderInfo.UID,
		HasUID:   leaderInfo.HasUID,
		SID:      leaderInfo.SID,
		JobToken: jobTok,
	}
	captureVerifiedGroupProof(&proof)
	defer closeVerifiedGroupProof(&proof)
	if _, ok := proof.verifiedFDs[c1PID]; !ok {
		t.Fatalf("expected cached pidfd for c1 %d in %+v", c1PID, proof.verifiedFDs)
	}

	// Fork uncaptured c2 and let leader exit.
	if err := os.WriteFile(releaseFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	var c2PID int
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(c2File); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && p > 0 {
				c2PID = p
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if c2PID <= 0 {
		t.Fatal("c2 did not start")
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(c2PID); err == nil {
			_ = p.Kill()
		}
	})

	// With 0 FDs free: post-exit cleanup MUST kill cached c1 via PidfdSendSignal,
	// MUST NOT use numeric kill(-PGID) (so uncaptured c2 remains alive), and MUST
	// return EMFILE rather than nil.
	cleanupZero := exhaustFDsUntilEMFILE(t)
	errZero := stopAdoptedProcessGroupAfterExit(proof)
	cleanupZero()
	if !isFDLimitErr(errZero) {
		t.Fatalf("0-FD post-exit cleanup err = %v, want EMFILE/ENFILE", errZero)
	}
	c1DeadBy := time.Now().Add(2 * time.Second)
	for time.Now().Before(c1DeadBy) && inspectProcess(c1PID).Alive {
		time.Sleep(15 * time.Millisecond)
	}
	if inspectProcess(c1PID).Alive {
		t.Fatalf("cached c1 pid %d survived post-exit EMFILE cleanup", c1PID)
	}
	if !inspectProcess(c2PID).Alive {
		t.Fatalf("uncaptured c2 pid %d was killed by numeric kill(-PGID) during 0-FD EMFILE cleanup", c2PID)
	}

	// With exactly 1 FD free: PidfdOpen on c2 consumes the last FD and /proc
	// revalidation hits EMFILE. Cleanup MUST preserve EMFILE rather than returning nil.
	cleanupOne := exhaustFDsLeaving(t, 1)
	errOne := stopAdoptedProcessGroupAfterExit(proof)
	cleanupOne()
	if !isFDLimitErr(errOne) {
		t.Fatalf("1-FD partial cleanup err = %v, want EMFILE/ENFILE", errOne)
	}
	if !inspectProcess(c2PID).Alive {
		t.Fatalf("c2 pid %d should still be alive after 1-FD revalidation EMFILE", c2PID)
	}
}

func TestCaptureVerifiedGroupProofPrunesExitedFDsLinux(t *testing.T) {
	root := t.TempDir()
	c1File := filepath.Join(root, "c1.pid")
	c1Rel := filepath.Join(root, "c1.rel")
	c2File := filepath.Join(root, "c2.pid")
	cmd := exec.CommandContext(t.Context(), "bash", "-c", fmt.Sprintf(
		"sleep 30 & c1=$!; echo $c1 > %q; while [ ! -f %q ]; do sleep 0.02; done; kill -9 $c1 2>/dev/null; wait $c1 2>/dev/null; sleep 30 & echo $! > %q; sleep 30",
		c1File, c1Rel, c2File,
	))
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = killProcessTree(cmd) })

	leaderPID := cmd.Process.Pid
	leaderInfo := inspectProcess(leaderPID)
	if !leaderInfo.Alive {
		t.Fatal("leader not inspectable")
	}
	var c1PID int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(c1File); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && p > 0 {
				c1PID = p
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if c1PID <= 0 {
		t.Fatal("c1 did not start")
	}

	proof := processGroupProof{
		Leader: ProcessIdentityInfo{PID: leaderPID, PGID: leaderInfo.PGID, StartToken: leaderInfo.StartToken},
		UID:    leaderInfo.UID,
		HasUID: leaderInfo.HasUID,
		SID:    leaderInfo.SID,
	}
	captureVerifiedGroupProof(&proof)
	defer closeVerifiedGroupProof(&proof)

	c1Entry, ok := proof.verifiedFDs[c1PID]
	if !ok || c1Entry.fd <= 0 {
		t.Fatalf("expected c1 %d in verifiedFDs: %+v", c1PID, proof.verifiedFDs)
	}

	if err := os.WriteFile(c1Rel, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var c2PID int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(c2File); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && p > 0 {
				c2PID = p
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if c2PID <= 0 {
		t.Fatal("c2 did not start")
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && inspectProcess(c1PID).Alive; {
		time.Sleep(15 * time.Millisecond)
	}

	captureVerifiedGroupProof(&proof)
	if _, stillCached := proof.verifiedFDs[c1PID]; stillCached {
		t.Fatalf("exited child c1 %d pidfd was not pruned from verifiedFDs", c1PID)
	}
	if _, historyKept := proof.KnownMembers[c1PID]; !historyKept {
		t.Fatalf("c1 %d historical KnownMembers entry should be preserved", c1PID)
	}
	if c2Entry, hasC2 := proof.verifiedFDs[c2PID]; !hasC2 || c2Entry.fd <= 0 {
		t.Fatalf("expected live child c2 %d in verifiedFDs", c2PID)
	}
	// Verify c1Entry.fd was actually closed in the process FD table.
	if _, err := unix.FcntlInt(uintptr(c1Entry.fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("pruned fd %d still open (fcntl err=%v)", c1Entry.fd, err)
	}
}

func TestStopAdoptedProcessGroupAfterExitTokenlessLegacyAndRetryLinux(t *testing.T) {
	root := t.TempDir()
	outFile := filepath.Join(root, "legacy.log")
	logF, err := os.OpenFile(outFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()

	// Part A: Tokenless child inheriting OutputFile (while closing FD 3) is positively
	// verified via /proc/<pid>/fd and stopped, and empty group is confirmed.
	c1File := filepath.Join(root, "c1.pid")
	cmdA := exec.CommandContext(t.Context(), "bash", "-c", fmt.Sprintf("(exec 3>&-; sleep 30) & echo $! > %q", c1File))
	configureProcess(cmdA)
	cmdA.Stdout = logF
	cmdA.Stderr = logF
	if err := cmdA.Start(); err != nil {
		t.Fatal(err)
	}
	leaderAPID := cmdA.Process.Pid
	leaderAInfo := inspectProcess(leaderAPID)
	_ = cmdA.Wait()
	b, err := os.ReadFile(c1File)
	if err != nil {
		t.Fatal(err)
	}
	c1PID, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	t.Cleanup(func() {
		if p, err := os.FindProcess(c1PID); err == nil {
			_ = p.Kill()
		}
	})

	proofA := processGroupProof{
		Leader:         ProcessIdentityInfo{PID: leaderAPID, PGID: leaderAInfo.PGID, StartToken: leaderAInfo.StartToken},
		UID:            leaderAInfo.UID,
		HasUID:         leaderAInfo.HasUID,
		SID:            leaderAInfo.SID,
		OutputFile:     outFile,
		KnownMembers:   map[int]string{leaderAPID: leaderAInfo.StartToken},
		VerifiedAt:     time.Now(),
		ExitObservedAt: time.Now(),
	}
	if err := stopAdoptedProcessGroupAfterExit(proofA); err != nil {
		t.Fatalf("tokenless OutputFile-backed cleanup err = %v, want nil", err)
	}
	if inspectProcess(c1PID).Alive {
		t.Fatalf("child pid %d survived tokenless OutputFile cleanup", c1PID)
	}

	// Part B: Tokenless child closing stdout/stderr (> /dev/null) with no KnownMembers/ppid
	// returns retryable errIncompleteGroupProof (NOT ErrProcessIdentityMismatch) when leader
	// was live-verified and exit observed, preserving /jobs stop retryability without killing blindly.
	c2File := filepath.Join(root, "c2.pid")
	cmdB := exec.CommandContext(t.Context(), "bash", "-c", fmt.Sprintf("(exec >/dev/null 2>&1; sleep 30) & echo $! > %q", c2File))
	configureProcess(cmdB)
	if err := cmdB.Start(); err != nil {
		t.Fatal(err)
	}
	leaderBPID := cmdB.Process.Pid
	leaderBInfo := inspectProcess(leaderBPID)
	_ = cmdB.Wait()
	b2, err := os.ReadFile(c2File)
	if err != nil {
		t.Fatal(err)
	}
	c2PID, _ := strconv.Atoi(strings.TrimSpace(string(b2)))
	t.Cleanup(func() {
		if p, err := os.FindProcess(c2PID); err == nil {
			_ = p.Kill()
		}
	})

	proofB := processGroupProof{
		Leader:         ProcessIdentityInfo{PID: leaderBPID, PGID: leaderBInfo.PGID, StartToken: leaderBInfo.StartToken},
		UID:            leaderBInfo.UID,
		HasUID:         leaderBInfo.HasUID,
		SID:            leaderBInfo.SID,
		OutputFile:     outFile,
		KnownMembers:   map[int]string{leaderBPID: leaderBInfo.StartToken},
		VerifiedAt:     time.Now(),
		ExitObservedAt: time.Now(),
	}
	errB := stopAdoptedProcessGroupAfterExit(proofB)
	if !errors.Is(errB, errIncompleteGroupProof) || errors.Is(errB, ErrProcessIdentityMismatch) {
		t.Fatalf("tokenless child without FD/ancestry err = %v, want errIncompleteGroupProof (not ErrProcessIdentityMismatch)", errB)
	}
	if !inspectProcess(c2PID).Alive {
		t.Fatalf("unverified child pid %d should not be killed blindly when proof is incomplete", c2PID)
	}
}
