package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitStatusDisablesRepositoryPrograms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init", "-b", "safe-status-branch")
	git("config", "core.fsmonitor", "touch monitor-ran")
	ex := &Executor{Root: root}
	res, err := ex.Dispatch(context.Background(), GitStatusTool, `{}`)
	if err != nil || !strings.Contains(res.Text, "safe-status-branch") {
		t.Fatalf("status = %q, %v", res.Text, err)
	}
	if _, err := os.Stat(filepath.Join(root, "monitor-ran")); !os.IsNotExist(err) {
		t.Fatalf("status executed repository fsmonitor: %v", err)
	}
	git("config", "filter.fixture.clean", "touch filter-ran")
	if _, err := ex.Dispatch(context.Background(), GitStatusTool, `{}`); err == nil || !strings.Contains(err.Error(), "Git filters") {
		t.Fatalf("status proceeded with a configured program: %v", err)
	}
	if _, err := ex.Dispatch(context.Background(), GitStatusTool, `{"cwd":"/"}`); err == nil {
		t.Fatal("status accepted a caller-controlled directory")
	}
	ex.UntrustedWorkspace = true
	if _, err := ex.Dispatch(context.Background(), GitStatusTool, `{}`); err == nil || !strings.Contains(err.Error(), "trusted workspace") {
		t.Fatalf("status accepted an untrusted workspace: %v", err)
	}
}
