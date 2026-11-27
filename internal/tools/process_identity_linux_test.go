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
