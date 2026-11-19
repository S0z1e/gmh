package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHeaderlessAdoptionRetryKeepsOriginalOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	id := "headerless-retry"
	want := filepath.Join(Dir(), id, "bash-output")
	if err := os.MkdirAll(want, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(want, "old.log"), []byte("original-output"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(), id+".jsonl")
	if err := os.WriteFile(path, []byte("{\"role\":\"user\",\"content\":\"old history\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := acquireWriter(id)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w, err = acquireWriterIn(filepath.Join(Dir(), ".output-writers"), id)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	// Permit existing files/claims to open, but fail the atomic header replacement.
	if err := os.Chmod(Dir(), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(Dir(), 0700)
	s, openErr := Open(id, OpenOptions{Workspace: root, Rebind: true})
	if s != nil {
		s.Close()
		t.Fatal("expected header replacement failure")
	}
	if openErr == nil {
		t.Fatal("expected header replacement failure")
	}
	claim, err := os.ReadFile(filepath.Join(Dir(), ".output-writers", id+".lock"))
	if err != nil || len(claim) != 0 {
		t.Fatalf("claim committed before adoption header: %q %v", claim, err)
	}
	if err := os.Chmod(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	s, err = Open(id, OpenOptions{Workspace: root, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.OutputDir() != want {
		t.Fatalf("retry orphaned original output: want %s, got %s; first failure: %v", want, s.OutputDir(), openErr)
	}
}

func TestPendingOutputClaimResumesStableAdoption(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("pending", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	h := *s.header
	h.OutputID = ""
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	path := s.Path
	s.Close()
	// Model a crash after durable adoption header + claim but before OutputID
	// installation. Retry must use this header's Created, rather than now.
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	claim, err := acquireOutputWriter(&h, "pending")
	if err != nil {
		t.Fatal(err)
	}
	claim.Close()
	s, err = Open("pending", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.OutputDir() != filepath.Join(Dir(), "pending", "bash-output") {
		t.Fatalf("lost pending claim: %s", s.OutputDir())
	}
	if !s.header.Created.Equal(h.Created) {
		t.Fatal("adoption identity changed")
	}
}
