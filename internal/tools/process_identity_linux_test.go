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
	proof.KnownMembers = captureVerifiedGroupMembers(proof)

	cleanupFDs := exhaustFDsUntilEMFILE(t)
	_, _, waitErr := waitAdoptedProcess(context.Background(), proof.Leader)
	if !errors.Is(waitErr, syscall.EMFILE) {
		cleanupFDs()
		t.Fatalf("waitAdoptedProcess under EMFILE err = %v, want EMFILE", waitErr)
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
}
