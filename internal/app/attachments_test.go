package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/agent"
	"minimalharness/internal/tools"
)

func TestAttachFileRejectsBinaryAndBoundsQueue(t *testing.T) {
	root := t.TempDir()
	a := App{Cfg: &agent.Config{Exec: &tools.Executor{Root: root}}}
	write := func(dir, name, data string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(root, "binary.dat", "text\x00")
	write(root, "large.log", strings.Repeat("x", tools.MaxTextAttachmentBytes+1))
	for _, path := range []string{"", ".", "binary.dat", "large.log"} {
		if _, err := a.AttachFile(path); err == nil {
			t.Errorf("AttachFile(%q) should fail", path)
		}
	}
	if len(a.PendingFiles) != 0 {
		t.Fatal("failed attachments entered the queue")
	}
	maxFile := write(t.TempDir(), "outside.log", strings.Repeat("x", tools.MaxTextAttachmentBytes))
	if _, err := a.AttachFile(maxFile); err != nil {
		t.Fatal(err)
	}
	write(root, "small.log", "x")
	if _, err := a.AttachFile("small.log"); err == nil || len(a.PendingFiles) != 1 {
		t.Fatalf("aggregate limit was not enforced: err=%v, queued=%d", err, len(a.PendingFiles))
	}
}
