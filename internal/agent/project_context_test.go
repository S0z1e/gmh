package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectContextFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing: empty section.
	if got := ProjectContext(root); got != "" {
		t.Fatalf("empty root = %q, want empty", got)
	}
	// AGENTS.md at the root is loaded.
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Be concise.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProjectContext(root); !strings.Contains(got, "Be concise.") || !strings.Contains(got, "Project context (AGENTS.md)") {
		t.Fatalf("agents.md = %q", got)
	}
	// CLAUDE.md alongside it is loaded too, after AGENTS.md.
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("Use tabs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ProjectContext(root)
	if !strings.Contains(got, "Use tabs.") || strings.Index(got, "Be concise.") > strings.Index(got, "Use tabs.") {
		t.Fatalf("both files = %q", got)
	}
	// A subdir root finds the parent's files (displayed by path).
	sub := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectContext(sub); !strings.Contains(got, "Be concise.") || !strings.Contains(got, "Project context (../../AGENTS.md)") {
		t.Fatalf("subdir root = %q", got)
	}
	// A root outside $HOME searches only itself, never /tmp or /.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "AGENTS.md"), []byte("Outside.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProjectContext(outside); !strings.Contains(got, "Outside.") {
		t.Fatalf("outside root = %q", got)
	}
}

func TestProjectContextTruncates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	big := strings.Repeat("x", 9*1024) + "TAIL-MARKER"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ProjectContext(root)
	if !strings.Contains(got, "[truncated") || strings.Contains(got, "TAIL-MARKER") {
		t.Fatal("oversize context file must be truncated with a marker")
	}
}

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
	if got := ProjectContext(root); strings.Contains(got, "Git status") {
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
	got := ProjectContext(root)
	for _, want := range []string{"branch: main", " M t.txt", "seed commit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("git snapshot = %q, want %q", got, want)
		}
	}
	// Clean tree says so.
	run("add", "t.txt")
	run("commit", "-m", "second")
	if got := ProjectContext(root); !strings.Contains(got, "working tree clean") {
		t.Fatalf("clean tree = %q", got)
	}
}
