package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOutputIdentitySurvivesRenameResumeAndNameReuse(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("original", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	dir := s.OutputDir()
	if err := s.Rename("renamed"); err != nil {
		t.Fatal(err)
	}
	if s.OutputDir() != dir {
		t.Fatal("rename moved output")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open("renamed", OpenOptions{Workspace: root, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.OutputDir() != dir {
		t.Fatal("resume/rebind lost output identity")
	}
	fresh, err := Open("original", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.OutputDir() == dir {
		t.Fatal("name reuse shares output")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "copy.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	copy, err := Open("copy", OpenOptions{Workspace: root})
	if copy != nil {
		copy.Close()
	}
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("copy admitted a second output writer: %v", err)
	}
}

func TestOutputIdentityAdoptsLegacyDirectory(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("old", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s.header.OutputID = ""
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(s.header); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	s.Close()
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RenameSession("old", "offline"); err != nil {
		t.Fatal(err)
	}
	s, err = Open("offline", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.OutputDir() != filepath.Join(Dir(), "old", "bash-output") {
		t.Fatalf("old artifacts orphaned: %s", s.OutputDir())
	}
	if err := s.Rename("new"); err != nil {
		t.Fatal(err)
	}
	if s.header.OutputID != "old" {
		t.Fatal("legacy output identity not persisted")
	}
}
