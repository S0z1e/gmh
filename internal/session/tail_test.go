package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"minimalharness/internal/llm"
)

func TestResumeRepairsTornRecordAtEveryByte(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	before := []byte("{\"role\":\"user\",\"content\":\"before\"}\n")
	record := []byte("{\"role\":\"assistant\",\"content\":\"answer 世界\"}\n")
	for cut := 0; cut <= len(record); cut++ {
		t.Run(strconv.Itoa(cut), func(t *testing.T) {
			id := "torn"
			if err := os.MkdirAll(Dir(), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(Dir(), id+".jsonl")
			original := append(bytes.Clone(before), record[:cut]...)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(id); err != nil {
				t.Fatal(err)
			}
			s, err := New(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Append(llm.Message{Role: "user", Content: "after"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			msgs, err := Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) < 2 || msgs[0].Content != "before" || msgs[len(msgs)-1].Content != "after" {
				t.Fatalf("cut=%d: lost append: %+v", cut, msgs)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(data, before) {
				t.Fatal("changed complete record")
			}
			if cut > 0 && cut < len(record)-1 {
				backups, err := filepath.Glob(path + "-torn-*")
				if err != nil || len(backups) == 0 {
					t.Fatalf("no torn-tail backup: %v", err)
				}
				backup, err := os.ReadFile(backups[len(backups)-1])
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, p := range backups {
					b, _ := os.ReadFile(p)
					if bytes.Equal(b, record[:cut]) {
						found = true
					}
				}
				if !found {
					t.Fatalf("tail not preserved: %q", backup)
				}
			}
		})
	}
}

func TestCorruptCompleteRecordIsNeverSilentlyDropped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(), "corrupt.jsonl")
	data := []byte("{\"role\":\"user\",\"content\":\"before\"}\n{broken}\n{\"role\":\"user\",\"content\":\"after\"}\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("corrupt"); err == nil {
		t.Fatal("Load silently skipped corruption")
	}
	if s, err := New("corrupt"); err == nil {
		s.Close()
		t.Fatal("opened corrupt session for writing")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("corrupt session modified: %v", err)
	}
}
