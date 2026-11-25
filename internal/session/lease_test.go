package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
)

func TestWriterLeaseSurvivesRewriteRenameAndClose(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("owned")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkBusy := func(id string) {
		t.Helper()
		other, err := New(id)
		if err == nil {
			other.Close()
			t.Fatalf("second writer opened %s", id)
		}
		if !strings.Contains(err.Error(), "already open for writing") {
			t.Fatal(err)
		}
	}
	checkBusy("owned")
	if err := s.Append(llm.Message{Role: "user", Content: "keep"}); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(s.ID); err != nil || len(got) != 1 {
		t.Fatalf("read-only reader: %v %v", got, err)
	}
	for _, action := range []func() error{
		func() error { return RenameSession("owned", "other") },
		func() error { return DeleteSession("owned") },
	} {
		if err := action(); err == nil {
			t.Fatal("changed another writer's session")
		}
	}
	if err := s.Rewrite([]llm.Message{{Role: "user", Content: "rewritten"}}); err != nil {
		t.Fatal(err)
	}
	checkBusy("owned")
	if err := s.Rename("renamed"); err != nil {
		t.Fatal(err)
	}
	checkBusy("renamed")
	// The old name is now free, but the new name stays owned.
	old, err := New("owned")
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	if err := s.Rename("bad/id"); err == nil {
		t.Fatal("invalid rename succeeded")
	}
	checkBusy("renamed")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New("renamed")
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}

func TestWriterLeaseCaseAliases(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("lower")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, aliasErr := os.Stat(filepath.Join(Dir(), "LOWER.jsonl"))
	other, err := New("LOWER")
	if aliasErr == nil {
		if err == nil {
			other.Close()
			t.Fatal("case alias bypassed lease")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		other.Close()
	}
}

func TestWriterLeaseProcessHelper(t *testing.T) {
	mode := os.Getenv("GMH_LEASE_HELPER")
	if mode == "" {
		return
	}
	// An empty GMH_LEASE_ROOT keeps the historical helper binding: older
	// tests open "shared" with New, which records the helper's own directory.
	workspace := os.Getenv("GMH_LEASE_ROOT")
	if workspace == "" {
		workspace = "."
	}
	s, err := Open("shared", OpenOptions{Workspace: workspace})
	if mode == "busy" {
		if err == nil {
			s.Close()
			t.Fatal("competing process opened session")
		}
		if !strings.Contains(err.Error(), "already open for writing") {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if mode == "dead" {
		if s.stopHeartbeat != nil {
			s.stopHeartbeat()
			s.stopHeartbeat = nil
		}
		host, _ := os.Hostname()
		dead := LockOwner{
			PID: 1<<30 - 1, Host: host,
			Started:        time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			Heartbeat:      time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			ProcessStarted: "dead-identity-that-cannot-match",
		}
		data, err := json.Marshal(dead)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeLockOwnerData(s.writer, data); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready")
		return // exiting releases the OS lock and leaves the dead owner record
	}
	if mode == "legacy" {
		started := time.Now().Add(-lockStaleTTL - time.Minute).UTC().Format(time.RFC3339)
		if err := writeLockOwnerData(s.writer, fmt.Appendf(nil, "pid=%d started=%s", os.Getpid(), started)); err != nil {
			t.Fatal(err)
		}
		fmt.Println(s.header.Workspace.Path)
	}
	if mode == "stall" {
		// Freeze the heartbeat before announcing ready, so a stealer observes
		// the stalled record and the refresh goroutine cannot rewrite it.
		if s.stopHeartbeat != nil {
			s.stopHeartbeat()
			s.stopHeartbeat = nil
		}
		owner, ok := parseLockOwner(s.writer)
		if !ok {
			t.Fatal("stall helper has no lock owner")
		}
		owner.Heartbeat = time.Now().Add(-lockStaleTTL - time.Minute).UTC().Format(time.RFC3339)
		data, err := json.Marshal(owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeLockOwnerData(s.writer, data); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("ready")
	if mode == "stall" {
		owner, _ := parseLockOwner(s.writer)
		fmt.Println(owner.ProcessStarted)
	}
	// Parent kills this process to prove the OS releases the lease after a crash.
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}

func TestWriterLeaseAcrossProcessesAndCrash(t *testing.T) {
	setTestHome(t, t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(mode string) *exec.Cmd {
		c := exec.Command(exe, "-test.run=^TestWriterLeaseProcessHelper$")
		c.Env = append(os.Environ(), "GMH_LEASE_HELPER="+mode, "GMH_LEASE_ROOT="+os.Getenv("GMH_LEASE_ROOT"))
		return c
	}
	s, err := New("shared")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := child("busy").CombinedOutput(); err != nil {
		s.Close()
		t.Fatalf("competing process: %v %s", err, out)
	}
	s.Close()
	c := child("hold")
	stdin, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	scanner := bufio.NewScanner(out)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("child did not acquire writer lease")
	}
	if err := c.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.Wait()
	next, err := New("shared")
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestWriterLeaseRejectsTranscriptLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			s, err := New("shared")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			alias := filepath.Join(Dir(), "alias.jsonl")
			if kind == "symlink" {
				err = os.Symlink(s.Path, alias)
			} else {
				err = os.Link(s.Path, alias)
			}
			if err != nil {
				t.Skipf("links unavailable: %v", err)
			}
			other, err := New("alias")
			if err == nil {
				other.Close()
				t.Fatal("transcript alias bypassed lease")
			}
		})
	}
}

func TestArchiveCannotOverwriteAnotherWriter(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.Append(llm.Message{Role: "user", Content: "keep"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveCopy(target.ID); err == nil {
		t.Fatal("overwrote active transcript")
	}
	target.Close()
	if _, err := s.ArchiveCopy(target.ID); err == nil {
		t.Fatal("overwrote closed transcript")
	}
	got, err := Load(target.ID)
	if err != nil || len(got) != 1 || got[0].Content != "keep" {
		t.Fatalf("archive damaged transcript: %v %v", got, err)
	}
}

func TestArchiveSupportsMaximumSessionID(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New(strings.Repeat("s", 128))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ArchiveCopy(s.ID + "-pre-compact-1234567890123456789"); err != nil {
		t.Fatal(err)
	}
}
