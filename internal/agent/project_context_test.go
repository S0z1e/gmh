package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectContextGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Not a repo: no git section, no error.
	if got := ProjectGitContext(root); strings.Contains(got, "Git status") {
		t.Fatalf("non-repo = %q", got)
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "t.txt"), []byte("theta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "t.txt")
	run("commit", "-m", "seed commit")
	// Dirty the tree after the commit.
	if err := os.WriteFile(filepath.Join(root, "t.txt"), []byte("theta\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ProjectGitContext(root)
	for _, want := range []string{"branch: main", " M t.txt", "seed commit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("git snapshot = %q, want %q", got, want)
		}
	}
	// Clean tree says so.
	run("add", "t.txt")
	run("commit", "-m", "second")
	if got := ProjectGitContext(root); !strings.Contains(got, "working tree clean") {
		t.Fatalf("clean tree = %q", got)
	}
}
