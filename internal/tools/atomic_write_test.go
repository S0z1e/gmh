package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteKeepsPinnedRootAfterPathReplacement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	moved := filepath.Join(base, "moved")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{workspace, outside} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, workspace); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(r, "f", []byte("updated")); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, moved, "f"); got != "updated" {
		t.Fatalf("pinned file = %q", got)
	}
	if got := readEditFile(t, outside, "f"); got != "original" {
		t.Fatalf("outside file changed: %q", got)
	}
	for _, dir := range []string{moved, outside} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("temporary files left in %s: %v, %v", dir, entries, err)
		}
	}
}

func TestAtomicWriteRejectsEscapingSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "f"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := atomicWriteFile(r, "alias", []byte("bad")); err == nil {
		t.Fatal("accepted symlink outside workspace")
	}
	if got := readEditFile(t, outside, "f"); got != "original" {
		t.Fatalf("outside file changed: %q", got)
	}
}
