package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
)

func writeMsgs(t *testing.T, s *Store, contents ...string) {
	t.Helper()
	for _, c := range contents {
		if err := s.Append(llm.Message{Role: "user", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old, err := New("old")
	if err != nil {
		t.Fatal(err)
	}
	writeMsgs(t, old, "first task about alpha", "second task")
	// Audit sidecar + pre-compact archive must not appear as sessions.
	if err := old.Audit(map[string]string{"kind": "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.ArchiveCopy("old-pre-compact-120000"); err != nil {
		t.Fatal(err)
	}
	old.Close()
	newer, err := New("newer")
	if err != nil {
		t.Fatal(err)
	}
	writeMsgs(t, newer, "a much later task with a very long first message that should be cut off at sixty runes for the preview")
	newer.Close()
	// Distinct mtimes so the order is deterministic.
	now := time.Now()
	os.Chtimes(filepath.Join(Dir(), "old.jsonl"), now.Add(-time.Hour), now.Add(-time.Hour))
	os.Chtimes(filepath.Join(Dir(), "newer.jsonl"), now, now)

	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %v, want 2 sessions", list)
	}
	if list[0].ID != "newer" || list[1].ID != "old" {
		t.Fatalf("order = %v, want newest first", list)
	}
	if list[0].Messages != 1 || list[1].Messages != 2 {
		t.Fatalf("counts = %v", list)
	}
	if list[1].Preview != "first task about alpha" {
		t.Fatalf("preview = %q", list[1].Preview)
	}
	if !strings.HasSuffix(list[0].Preview, "…") || len([]rune(list[0].Preview)) > 61 {
		t.Fatalf("long preview not truncated: %q", list[0].Preview)
	}
	// A /clear marker resets the count and the preview, like Load.
	cleared, err := New("cleared")
	if err != nil {
		t.Fatal(err)
	}
	writeMsgs(t, cleared, "before clear")
	if err := cleared.Clear(); err != nil {
		t.Fatal(err)
	}
	writeMsgs(t, cleared, "after clear")
	cleared.Close()
	list, err = List()
	if err != nil {
		t.Fatal(err)
	}
	var got *Info
	for i := range list {
		if list[i].ID == "cleared" {
			got = &list[i]
		}
	}
	if got == nil || got.Messages != 1 || got.Preview != "after clear" {
		t.Fatalf("cleared session = %+v", got)
	}
}

func TestValidateSessionID(t *testing.T) {
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", "a\\b", "a b", "a:b", strings.Repeat("x", 129)} {
		if err := ValidateSessionID(bad); err == nil {
			t.Fatalf("id %q should be invalid", bad)
		}
	}
	for _, good := range []string{"20261110-105344", "s-alpha", "a.b_c-1"} {
		if err := ValidateSessionID(good); err != nil {
			t.Fatalf("id %q should be valid: %v", good, err)
		}
	}
}

func TestRenameSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("orig")
	if err != nil {
		t.Fatal(err)
	}
	writeMsgs(t, s, "hello")
	if err := s.Audit(map[string]string{"kind": "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveCopy("orig-pre-compact-120000"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if err := RenameSession("nope", "x"); err == nil || !strings.Contains(err.Error(), "unknown session") {
		t.Fatalf("rename unknown err = %v", err)
	}
	if err := RenameSession("orig", "bad/id"); err == nil {
		t.Fatal("rename to an invalid id should fail")
	}
	if err := RenameSession("orig", "orig"); err == nil {
		t.Fatal("rename to the same id should fail")
	}
	if err := RenameSession("orig", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "orig.jsonl")); !os.IsNotExist(err) {
		t.Fatal("old transcript should be gone")
	}
	for _, want := range []string{"renamed.jsonl", "renamed-audit.jsonl", "renamed-pre-compact-120000.jsonl"} {
		if _, err := os.Stat(filepath.Join(Dir(), want)); err != nil {
			t.Fatalf("renamed file missing: %s", want)
		}
	}
	if err := RenameSession("renamed", "renamed"); err == nil {
		t.Fatal("rename to the same id should fail")
	}
	other, err := New("other")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err := RenameSession("renamed", "other"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("rename onto an existing session err = %v", err)
	}
	// Load the renamed transcript: the message survived the move.
	msgs, err := Load("renamed")
	if err != nil || len(msgs) != 1 || msgs[0].Content != "hello" {
		t.Fatalf("load after rename = %v, err %v", msgs, err)
	}
}

func TestStoreRename(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("live")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	writeMsgs(t, s, "before rename")
	if err := s.Rename("live2"); err != nil {
		t.Fatal(err)
	}
	if s.ID != "live2" {
		t.Fatalf("store id = %q", s.ID)
	}
	// Appends after the rename land in the new file.
	writeMsgs(t, s, "after rename")
	if _, err := os.Stat(filepath.Join(Dir(), "live.jsonl")); !os.IsNotExist(err) {
		t.Fatal("old transcript should be gone")
	}
	data, err := os.ReadFile(filepath.Join(Dir(), "live2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "before rename") || !strings.Contains(string(data), "after rename") {
		t.Fatalf("renamed transcript = %s", data)
	}
	if err := s.Rename("bad/id"); err == nil {
		t.Fatal("rename to an invalid id should fail")
	}
	// A failed rename leaves the store usable on its old file.
	writeMsgs(t, s, "still here")
	data, err = os.ReadFile(filepath.Join(Dir(), "live2.jsonl"))
	if err != nil || !strings.Contains(string(data), "still here") {
		t.Fatalf("store after failed rename = %s, err %v", data, err)
	}
}
