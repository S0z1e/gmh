package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/llm"
)

func TestStealSessionHoldsLeaseAgainstCompetingWriter(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestWriterLeaseProcessHelper$")
	child.Env = append(os.Environ(), "GMH_LEASE_HELPER=hold", "GMH_LEASE_ROOT="+root, "HOME="+home, "USERPROFILE="+home)
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
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	line := bufio.NewScanner(stdout)
	if !line.Scan() || line.Text() != "ready" {
		t.Fatalf("holder did not start: %q (%v)", line.Text(), line.Err())
	}

	busy, err := Open("shared", OpenOptions{Workspace: root, Existing: true})
	if err == nil {
		busy.Close()
		t.Fatal("ordinary open stole a live writer")
	}
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ordinary open = %v, want ErrSessionBusy", err)
	}

	if _, err := StealSession("shared", root, false); err == nil || !errors.Is(err, ErrSessionBusy) || !strings.Contains(err.Error(), "still active") {
		t.Fatalf("fresh owner steal = %v, want an active-owner refusal", err)
	}

	// A dead owner (PID that cannot be signalled and is not us) releases the
	// claim only after the recorded identity is proven gone. While the child
	// still holds the lock, a second process must not be able to open it.
	contender := exec.Command(exe, "-test.run=^TestWriterLeaseProcessHelper$")
	contender.Env = append(os.Environ(), "GMH_LEASE_HELPER=busy", "GMH_LEASE_ROOT="+root, "HOME="+home, "USERPROFILE="+home)
	out, err := contender.CombinedOutput()
	if err != nil {
		t.Fatalf("contender during live hold: %v %s", err, out)
	}
}

func TestStealSessionTerminatesStalledOwnerAndKeepsLease(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestWriterLeaseProcessHelper$")
	child.Env = append(os.Environ(), "GMH_LEASE_HELPER=stall", "GMH_LEASE_ROOT="+root, "HOME="+home, "USERPROFILE="+home)
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
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	line := bufio.NewScanner(stdout)
	if !line.Scan() || line.Text() != "ready" {
		t.Fatalf("stalled writer did not start: %q (%v)", line.Text(), line.Err())
	}
	if !line.Scan() || line.Text() == "" {
		t.Fatalf("stalled writer did not publish its process identity: %q", line.Text())
	}

	var slipped atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			other, err := Open("shared", OpenOptions{Workspace: root, Existing: true})
			if err == nil {
				other.Close()
				slipped.Store(true)
				return
			}
		}
	}()
	stolen, err := StealSession("shared", root, false)
	if slipped.Load() {
		close(stop)
		<-done
		t.Fatal("another writer opened the session during the steal")
	}
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("steal stalled owner: %v", err)
	}
	defer stolen.Close()
	// The handoff must already own the lease: a competing writer cannot enter
	// between the steal and the reopen, because they are the same open.
	contender := exec.Command(exe, "-test.run=^TestWriterLeaseProcessHelper$")
	contender.Env = append(os.Environ(), "GMH_LEASE_HELPER=busy", "GMH_LEASE_ROOT="+root, "HOME="+home, "USERPROFILE="+home)
	out, err := contender.CombinedOutput()
	if err != nil {
		t.Fatalf("competing writer after steal: %v %s", err, out)
	}
	if err := stolen.Append(llm.Message{Role: "user", Content: "after steal"}); err != nil {
		t.Fatal(err)
	}
	got, err := Load("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "after steal" {
		t.Fatalf("reopened transcript = %+v", got)
	}
}

func TestStealSessionRefusesUnprovenOwner(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("unproven", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner := LockOwner{
		PID:            os.Getpid(),
		Host:           mustHostname(t),
		Started:        time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		Heartbeat:      time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		ProcessStarted: "",
	}
	data, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLockOwnerData(s.writer, data); err != nil {
		t.Fatal(err)
	}
	// The record names this process but carries no process identity, so the
	// steal must refuse instead of signalling it. Use a PID that is not us so
	// the current-process check is not the reason for the refusal: a PID of 1
	// with a blank identity is still unproven.
	owner.PID = 1
	data, _ = json.Marshal(owner)
	if err := writeLockOwnerData(s.writer, data); err != nil {
		t.Fatal(err)
	}
	if _, err := StealSession("unproven", root, false); err == nil || !errors.Is(err, ErrSessionBusy) || !strings.Contains(err.Error(), "cannot prove process identity") {
		t.Fatalf("unproven steal = %v, want an identity refusal", err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "unproven.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestStealSessionDoesNotCreateLockForMissingTranscript(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	if _, err := StealSession("missing-session", root, false); !os.IsNotExist(err) {
		t.Fatalf("missing session = %v", err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), ".writers", "missing-session.lock")); !os.IsNotExist(err) {
		t.Fatalf("steal created a lock for a missing session: %v", err)
	}
}

func TestStealSessionRecoversDeadOwner(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	root := t.TempDir()
	seed, err := Open("shared", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Append(llm.Message{Role: "user", Content: "kept"}); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestWriterLeaseProcessHelper$")
	child.Env = append(os.Environ(), "GMH_LEASE_HELPER=dead", "GMH_LEASE_ROOT="+root, "HOME="+home, "USERPROFILE="+home)
	out, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ready") {
		t.Fatalf("dead-owner helper: %v %s", err, out)
	}

	stolen, err := StealSession("shared", root, false)
	if err != nil {
		t.Fatalf("dead owner steal: %v", err)
	}
	defer stolen.Close()
	got, err := Load("shared")
	if err != nil || len(got) != 1 || got[0].Content != "kept" {
		t.Fatalf("recovered history = %+v %v", got, err)
	}
	contender := exec.Command(exe, "-test.run=^TestWriterLeaseProcessHelper$")
	contender.Env = append(os.Environ(), "GMH_LEASE_HELPER=busy", "GMH_LEASE_ROOT="+root, "HOME="+home, "USERPROFILE="+home)
	if out, err = contender.CombinedOutput(); err != nil {
		t.Fatalf("competing writer after dead-owner steal: %v %s", err, out)
	}
}

func mustHostname(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatalf("hostname: %v %q", err, host)
	}
	return host
}
