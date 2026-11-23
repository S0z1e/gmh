//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTextAttachmentResolvesParentAfterSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inner := filepath.Join(outside, "inner")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inner, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server.log"), []byte("workspace log"), 0o600); err != nil {
		t.Fatal(err)
	}
	const snapshot = "selected log"
	if err := os.WriteFile(filepath.Join(outside, "server.log"), []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := Executor{Root: root}
	for _, path := range []string{"linked/../server.log", root + "/linked/../server.log"} {
		file, err := exec.LoadTextAttachment(path)
		if err != nil || file.Path != path || file.Content != snapshot {
			t.Errorf("LoadTextAttachment(%q) = %+v, %v", path, file, err)
		}
	}
}
