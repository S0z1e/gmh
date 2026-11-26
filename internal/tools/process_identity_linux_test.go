//go:build linux

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func exhaustFDsUntilEMFILE(t *testing.T) func() {
	t.Helper()
	var openFDs []*os.File
	for range 4096 {
		f, err := os.Open("/dev/null")
		if err != nil {
			if errors.Is(err, syscall.EMFILE) {
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
