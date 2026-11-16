package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRejectsSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("outside-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Root: root}
	checks := []struct {
		name string
		run  func() error
	}{
		{"read", func() error { _, err := e.Read(ReadArgs{Path: "escape/secret.txt"}); return err }},
		{"write", func() error { _, err := e.Write(WriteArgs{Path: "escape/new.txt", Content: "bad"}); return err }},
		{"edit", func() error {
			_, err := e.Edit(EditArgs{Path: "escape/secret.txt", OldText: "outside-marker", NewText: "bad"})
			return err
		}},
		{"grep", func() error {
			_, _, err := e.Grep(context.Background(), GrepArgs{Path: "escape", Pattern: "outside-marker"})
			return err
		}},
		{"glob", func() error {
			_, _, err := e.Glob(context.Background(), GlobArgs{Path: "escape", Pattern: "*"})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err == nil {
				t.Fatal("accepted a symlink outside workspace")
			}
		})
	}
	data, _ := os.ReadFile(target)
	if string(data) != "outside-marker" {
		t.Fatal("outside file changed")
	}
}

func TestWorkspaceAllowsInternalSymlink(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "real"), 0700)
	os.WriteFile(filepath.Join(root, "real", "f"), []byte("safe"), 0600)
	os.Symlink("real", filepath.Join(root, "link"))
	e := &Executor{Root: root}
	out, err := e.Read(ReadArgs{Path: "link/f"})
	if err != nil || !strings.Contains(out, "safe") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestReadPagesBeyondFirstMiB(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("1234567\n", 150000)+"tail-marker\n"), 0600)
	out, err := (&Executor{Root: root}).Read(ReadArgs{Path: "large.txt", Offset: 150001, Limit: 1})
	if err != nil || !strings.Contains(out, "tail-marker") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestWorkspaceWritesPreserveFileSymlinks(t *testing.T) {
	for _, operation := range []string{"edit", "overwrite"} {
		for _, linkTarget := range []string{"real/f", "nested/next", "dir-link/../f"} {
			t.Run(operation+"/"+linkTarget, func(t *testing.T) {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, "real", "sub"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "real", "f"), []byte("old\n"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../real/f", filepath.Join(root, "nested", "next")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("real/sub", filepath.Join(root, "dir-link")); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(linkTarget, alias); err != nil {
					t.Fatal(err)
				}
				e := &Executor{Root: root}
				var err error
				if operation == "edit" {
					_, err = e.Edit(EditArgs{Path: "alias", OldText: "old", NewText: "new"})
				} else {
					_, err = e.Write(WriteArgs{Path: "alias", Content: "new\n", Overwrite: true})
				}
				if err != nil {
					t.Fatal(err)
				}
				if target, err := os.Readlink(alias); err != nil || target != linkTarget {
					t.Fatalf("symlink = %q, %v", target, err)
				}
				if got := readEditFile(t, root, "real/f"); got != "new\n" {
					t.Fatalf("target = %q", got)
				}
				st, err := os.Stat(alias)
				if err != nil || st.Mode().Perm() != 0755 {
					t.Fatalf("target mode changed: %v, %v", st, err)
				}
			})
		}
	}
}
