package session

import (
	"minimalharness/internal/llm"
	"os"
	"testing"
)

func TestSessionArtifactsArePrivate(t *testing.T) {
	setTestHome(t, t.TempDir())
	for _, id := range []string{"", "explicit"} {
		s, err := New(id)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Append(llm.Message{Role: "user", Content: "private"}); err != nil {
			t.Fatal(err)
		}
		if err = s.Audit(map[string]string{"secret": "value"}); err != nil {
			t.Fatal(err)
		}
		archive, err := s.ArchiveCopy(s.ID + "-archive")
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{s.Path, s.AuditPath(), archive} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("path=%s mode=%v err=%v", path, info.Mode(), err)
			}
		}
		_ = s.Close()
		if err := os.Chmod(s.Path, 0644); err != nil {
			t.Fatal(err)
		}
		reopened, err := New(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(reopened.Path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("existing transcript permissions not repaired")
		}
		_ = reopened.Close()
		_ = s.Close()
	}
}
