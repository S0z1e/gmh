package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/tools"
)

func TestProjectGitStatusIsObservedOnDemand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OPENAI_API_KEY", "fake")
	root := t.TempDir()
	writeMockProvider(t, root)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init", "-b", "fixture-start-branch")
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	const path = "fixture-created-after-setup.txt"
	if err := os.WriteFile(filepath.Join(root, path), []byte("untracked fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	git("symbolic-ref", "HEAD", "refs/heads/fixture-current-branch")
	if strings.Contains(a.Cfg.System, "fixture-start-branch") || strings.Contains(a.Cfg.System, "fixture-current-branch") || strings.Contains(a.Cfg.System, path) {
		t.Fatal("system prompt contains snapshot data for this checkout")
	}
	result, err := a.Cfg.Exec.Dispatch(context.Background(), tools.GitStatusTool, `{}`)
	if err != nil || !strings.Contains(result.Text, "fixture-current-branch") || !strings.Contains(result.Text, path) || strings.Contains(result.Text, "fixture-start-branch") {
		t.Fatalf("on-demand status = %q, %v", result.Text, err)
	}
}

func TestProjectGitStatusReadsExternalLinkedWorktreeMetadata(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := t.TempDir()
	root := filepath.Join(t.TempDir(), "linked")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = base
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "initial")
	git("worktree", "add", "-b", "linked-branch", root)
	writeMockProvider(t, root)
	t.Setenv("OPENAI_API_KEY", "fake")
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	// Bash would fail to start with this sandbox configuration. The fixed
	// status lookup can resolve linked metadata without touching Bash.
	a.Cfg.Exec.Sandbox = true
	a.Cfg.Exec.BwrapPath = filepath.Join(t.TempDir(), "missing-bwrap")
	result, err := a.Cfg.Exec.Dispatch(context.Background(), tools.GitStatusTool, `{}`)
	if err != nil || !strings.Contains(result.Text, "linked-branch") || !strings.Contains(result.Text, "?? .harness.json") {
		t.Fatalf("linked worktree status = %q, %v", result.Text, err)
	}
}
