package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLockOwnerDiagnosticsAndRemoteHostRefusal(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("lock-diag", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Concurrent open reports alive owner with pid.
	_, err = Open("lock-diag", OpenOptions{Workspace: root})
	if err == nil || !strings.Contains(err.Error(), "status=alive") {
		t.Fatalf("expected alive lock diagnostic, got %v", err)
	}

	// StealWriterLock refuses to steal from current process.
	if err := StealWriterLock("lock-diag", root); err == nil || !strings.Contains(err.Error(), "current process") {
		t.Fatalf("expected refusal for current process, got %v", err)
	}

	// Overwrite lock metadata with a remote host + stale heartbeat and verify
	// StealWriterLock refuses to signal a PID from another host.
	remote := LockOwner{
		PID:       999999,
		Started:   time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		Heartbeat: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		Host:      "remote-host-never-matches.invalid",
	}
	data, _ := json.Marshal(remote)
	if err := writeLockOwnerData(s.writer, data); err != nil {
		t.Fatal(err)
	}
	if err := StealWriterLock("lock-diag", root); err == nil || !strings.Contains(err.Error(), "remote host") {
		t.Fatalf("expected remote host refusal, got %v", err)
	}
	if err := StealWriterLock("lock-diag", t.TempDir()); err == nil || !strings.Contains(err.Error(), "workspace/repository differs") || errors.Is(err, ErrSessionBusy) {
		t.Fatalf("expected workspace refusal before examining the owner, got %v", err)
	}
}

func TestLockStalenessRequiresHeartbeat(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-10 * time.Minute).Format(time.RFC3339)
	owner := LockOwner{Started: old}
	if lockOwnerStale(owner, now) {
		t.Fatal("legacy start time counted as a stale heartbeat")
	}
	owner.Heartbeat = old
	if !lockOwnerStale(owner, now) {
		t.Fatal("old heartbeat was not stale")
	}
}

func TestStealWriterLockRefusesLiveLegacyWriter(t *testing.T) {
	setTestHome(t, t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), exe, "-test.run=^TestWriterLeaseProcessHelper$")
	child.Env = append(os.Environ(), "GMH_LEASE_HELPER=legacy")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = child.Wait() })
	line := bufio.NewScanner(stdout)
	if !line.Scan() || line.Text() != "ready" {
		t.Fatalf("legacy writer did not start: %q (%v)", line.Text(), line.Err())
	}
	if err := StealWriterLock("shared", ""); !errors.Is(err, ErrSessionBusy) || !strings.Contains(err.Error(), "cannot verify heartbeat") {
		t.Fatalf("expected legacy writer refusal, got %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("legacy writer did not shut down normally: %v", err)
	}
	if err := StealWriterLock("shared", ""); err != nil {
		t.Fatalf("could not recover free lease: %v", err)
	}
}
