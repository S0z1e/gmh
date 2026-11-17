package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"minimalharness/internal/llm"
)

type failedRecordReader struct {
	data []byte
	err  error
}

func (r *failedRecordReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestRecordReadFailureIsNotCrashDamage(t *testing.T) {
	want := errors.New("injected read failure")
	for _, data := range []string{`{"role":"user","content":"partial`, `{"role":"user","content":"complete"}`} {
		_, _, torn, err := scanRecords(&failedRecordReader{data: []byte(data), err: want}, nil)
		if !errors.Is(err, want) || torn {
			t.Fatalf("torn=%v err=%v; want read failure", torn, err)
		}
	}
}

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

func TestConcurrentRecoveryKeepsSuccessfulAppends(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		id := fmt.Sprintf("race-%d", attempt)
		path := filepath.Join(Dir(), id+".jsonl")
		original := append([]byte("{\"role\":\"user\",\"content\":\"before\"}\n{\"role\":\"assistant\",\"content\":\""), bytes.Repeat([]byte("x"), 2*1024*1024)...)
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for n := 0; n < 2; n++ {
			wg.Go(func() {
				<-start
				s, err := New(id)
				if err != nil {
					errs <- err
					return
				}
				defer s.Close()
				errs <- s.Append(llm.Message{Role: "user", Content: fmt.Sprintf("after-%d", n)})
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		successes := 0
		for err := range errs {
			if errors.Is(err, ErrSessionBusy) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			successes++
		}
		msgs, err := Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if successes == 0 || len(msgs) != 1+successes {
			t.Fatalf("attempt %d: %d appends succeeded but got %d messages", attempt, successes, len(msgs))
		}
	}
}

func TestDeleteRemovesTornBackup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(), "deleted.jsonl")
	if err := os.WriteFile(path, []byte("{\"role\":\"assistant\",\"content\":\"private"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New("deleted")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := DeleteSession("deleted"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".writers" {
			t.Fatalf("deleted session left recovered private content: %s", entry.Name())
		}
	}
}

func TestRenameMovesTornBackups(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(), "old.jsonl")
	if err := os.WriteFile(path, []byte(`{"role":"user","content":"private`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New("old")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Rename("new"); err != nil {
		t.Fatal(err)
	}
	old, _ := filepath.Glob(path + "-torn-*")
	backups, _ := filepath.Glob(s.Path + "-torn-*")
	if len(old) != 0 || len(backups) != 1 {
		t.Fatalf("old=%v new=%v", old, backups)
	}
	if err := s.Append(llm.Message{Role: "user", Content: "after"}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupLifecycleWithLiteralGlobCharacters(t *testing.T) {
	for _, action := range []string{"rename", "delete"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("HOME", filepath.Join(t.TempDir(), "home[1]["))
			if err := os.MkdirAll(Dir(), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(Dir(), "old.jsonl")
			if err := os.WriteFile(path, []byte(`{"role":"user","content":"private`), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := New("old")
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			if action == "rename" {
				err = RenameSession("old", "new")
			} else {
				err = DeleteSession("old")
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(Dir())
			if err != nil {
				t.Fatal(err)
			}
			for _, en := range entries {
				if strings.HasPrefix(en.Name(), "old.jsonl-torn-") {
					t.Fatalf("%s returned success but left private backup %s", action, en.Name())
				}
			}
		})
	}
}

func TestBackupCleanupPreservesSimilarlyNamedSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("old")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	other, err := New("old.jsonl-torn-keep")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Append(llm.Message{Role: "user", Content: "keep"}); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err := DeleteSession("old"); err != nil {
		t.Fatal(err)
	}
	msgs, err := Load(other.ID)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "keep" {
		t.Fatalf("other session changed: %+v %v", msgs, err)
	}
}

func TestBackupLifecyclePreservesOtherSessionsBackups(t *testing.T) {
	for _, action := range []string{"rename", "delete"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := os.MkdirAll(Dir(), 0700); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"old", "old.jsonl-torn-keep"} {
				path := filepath.Join(Dir(), id+".jsonl")
				if err := os.WriteFile(path, []byte(`{"role":"user","content":"private`), 0600); err != nil {
					t.Fatal(err)
				}
				s, err := New(id)
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
			}
			otherPath := filepath.Join(Dir(), "old.jsonl-torn-keep.jsonl")
			other, err := recoveryBackups(otherPath)
			if err != nil || len(other) != 1 {
				t.Fatalf("other backup missing: %v %v", other, err)
			}
			want, err := os.ReadFile(other[0])
			if err != nil {
				t.Fatal(err)
			}
			if action == "rename" {
				err = RenameSession("old", "new")
			} else {
				err = DeleteSession("old")
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(other[0])
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("other backup changed: %q %v", got, err)
			}
		})
	}
}

func TestBackupLifecycleRecognizesCaseAliases(t *testing.T) {
	for _, action := range []string{"rename", "delete"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := os.MkdirAll(Dir(), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(Dir(), "lower.jsonl")
			if err := os.WriteFile(path, []byte(`{"role":"user","content":"private`), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(Dir(), "LOWER.jsonl")); os.IsNotExist(err) {
				t.Skip("case-sensitive filesystem")
			}
			s, err := New("LOWER")
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			if action == "rename" {
				err = RenameSession("lower", "new")
			} else {
				err = DeleteSession("lower")
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(Dir())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(strings.ToLower(entry.Name()), "lower.jsonl-torn-") {
					t.Fatalf("backup left behind: %s", entry.Name())
				}
			}
		})
	}
}

func TestBackupLifecyclePreservesCaseDistinctSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"lower", "LOWER"} {
		path := filepath.Join(Dir(), id+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			t.Skip("case-insensitive filesystem")
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.WriteString(`{"role":"user","content":"private`); err != nil {
			t.Fatal(err)
		}
		f.Close()
		s, err := New(id)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	other, err := recoveryBackups(filepath.Join(Dir(), "LOWER.jsonl"))
	if err != nil || len(other) != 1 {
		t.Fatalf("other=%v err=%v", other, err)
	}
	if err := DeleteSession("lower"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other[0]); err != nil {
		t.Fatalf("other backup removed: %v", err)
	}
}
