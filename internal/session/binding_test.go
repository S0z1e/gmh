package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/workspace"
)

func TestBindingHeaderPersistsWithoutEnteringHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open("bound", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	created := s.header.Created
	if s.header.Repository == nil || s.header.Workspace.Path == "" {
		t.Fatalf("header=%+v", s.header)
	}
	for _, action := range []func() error{
		func() error { return s.Append(llm.Message{Role: "user", Content: "before"}) },
		s.Clear,
		func() error { return s.MarkCompact("summary") },
		func() error { return s.Rewrite([]llm.Message{{Role: "user", Content: "after"}}) },
		func() error { return s.Rename("renamed") },
	} {
		if err := action(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load(s.ID)
	if err != nil || len(got) != 1 || got[0].Content != "after" {
		t.Fatalf("history=%+v err=%v", got, err)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.SplitN(data, []byte{'\n'}, 2)[0]
	h, err := decodeHeader(first)
	if err != nil || !h.Created.Equal(created) {
		t.Fatalf("header changed: %s %v", first, err)
	}
	s.Close()
	next, err := Open("renamed", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestBindingLegacyRequiresExplicitAdoptionAndKeepsRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := newStore("legacy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(llm.Message{Role: "user", Content: "legacy task"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open("legacy", OpenOptions{Workspace: root}); err == nil {
		other.Close()
		t.Fatal("silently adopted legacy history")
	}
	after, _ := os.ReadFile(s.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected adoption mutated history")
	}
	adopted, err := Open("legacy", OpenOptions{Workspace: root, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	adopted.Close()
	after, _ = os.ReadFile(s.Path)
	if !bytes.HasSuffix(after, before) {
		t.Fatal("legacy records were rewritten")
	}
	got, err := Load("legacy")
	if err != nil || len(got) != 1 || got[0].Content != "legacy task" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestBindingWrongWorkspaceRejectsBeforeTailRepair(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	s, err := Open("bound", OpenOptions{Workspace: a})
	if err != nil {
		t.Fatal(err)
	}
	created := s.header.Created
	s.Close()
	f, err := os.OpenFile(s.Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"role":"user","content":"torn`)
	f.Close()
	before, _ := os.ReadFile(s.Path)
	if other, err := Open("bound", OpenOptions{Workspace: b}); err == nil {
		other.Close()
		t.Fatal("wrong workspace opened")
	}
	after, _ := os.ReadFile(s.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("wrong workspace repaired tail")
	}
	backups, _ := recoveryBackups(s.Path)
	if len(backups) != 0 {
		t.Fatal("rejected open wrote backups")
	}
	moved, err := Open("bound", OpenOptions{Workspace: b, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	if !moved.header.Created.Equal(created) {
		t.Fatal("rebind lost creation time")
	}
	moved.Close()
	resumed, err := Open("bound", OpenOptions{Workspace: b})
	if err != nil {
		t.Fatal(err)
	}
	resumed.Close()
	if other, err := Open("bound", OpenOptions{Workspace: a}); err == nil {
		other.Close()
		t.Fatal("old binding retained")
	}
}

func TestBindingRejectsUnknownVersionEvenWithRebind(t *testing.T) {
	for _, change := range []string{"version", "created", "workspace"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			s, err := Open("bound", OpenOptions{Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			data, _ := os.ReadFile(s.Path)
			var h map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(data), &h); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "version":
				h["version"] = 999
			case "created":
				delete(h, "created")
			case "workspace":
				h["workspace"] = map[string]string{"path": "relative"}
			}
			invalid, _ := json.Marshal(h)
			invalid = append(invalid, '\n')
			if err := os.WriteFile(s.Path, invalid, 0600); err != nil {
				t.Fatal(err)
			}
			for _, rebind := range []bool{false, true} {
				if other, err := Open("bound", OpenOptions{Workspace: root, Rebind: rebind}); err == nil {
					other.Close()
					t.Fatal("invalid header accepted")
				}
			}
			if _, err := Load("bound"); err == nil {
				t.Fatal("viewer accepted invalid schema")
			}
			after, _ := os.ReadFile(s.Path)
			if !bytes.Equal(invalid, after) {
				t.Fatal("invalid header mutated")
			}
		})
	}
}

func TestBindingAliasesReplacementAndRepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open("bound", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(root, alias); err == nil {
		next, err := Open("bound", OpenOptions{Workspace: alias})
		if err != nil {
			t.Fatal(err)
		}
		next.Close()
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if other, err := Open("bound", OpenOptions{Workspace: root}); err == nil {
		other.Close()
		t.Fatal("repository changed without rebind")
	}
	rebound, err := Open("bound", OpenOptions{Workspace: root, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	rebound.Close()
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if other, err := Open("bound", OpenOptions{Workspace: root}); err == nil {
		other.Close()
		t.Fatal("replaced directory inherited session")
	}
}

func TestBindingWithoutCreationIdentityRequiresExplicitRebind(t *testing.T) {
	id := workspace.Identity{Path: filepath.Clean(t.TempDir())}
	h := &Header{Workspace: id}
	if err := checkBinding(h, id, nil); err == nil || !strings.Contains(err.Error(), "rebind-session") {
		t.Fatalf("missing creation identity: %v", err)
	}
}

func TestRepositoryIdentityReadsGitFileWithoutExecutingGit(t *testing.T) {
	root := t.TempDir()
	gitdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+gitdir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := repositoryIdentity(root)
	want, err := workspace.Identify(gitdir)
	if err != nil || repo == nil || !repo.Matches(want) {
		t.Fatalf("repo=%v err=%v", repo, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte(strings.Repeat("x", maxHeaderBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if repo := repositoryIdentity(root); repo != nil {
		t.Fatal("oversized gitdir file accepted")
	}
}

func TestBindingCanExplicitlyImportAnotherOSPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("copied", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	h := *s.header
	s.Close()
	h.Workspace.Path = `C:\copied\project`
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("copied"); err != nil {
		t.Fatal(err)
	}
	rebound, err := Open("copied", OpenOptions{Workspace: root, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	rebound.Close()
}
