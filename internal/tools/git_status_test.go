package tools

import (
	"context"
	"io"
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
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("tracked filter=fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(root, "tracked")
	if err := os.WriteFile(tracked, []byte("tracked content"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-c", "core.fsmonitor=", "add", "tracked")
	git("config", "filter.fixture.clean", "touch filter-ran; cat")
	if err := os.WriteFile(tracked, []byte("different content after staging"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err = ex.Dispatch(context.Background(), GitStatusTool, `{}`)
	if err != nil || !strings.Contains(res.Text, "AM tracked") || !strings.Contains(res.Text, "does not inspect submodule working trees") {
		t.Fatalf("filtered repo status = %q, %v", res.Text, err)
	}
	if _, err := os.Stat(filepath.Join(root, "filter-ran")); !os.IsNotExist(err) {
		t.Fatalf("status executed a repository filter: %v", err)
	}
	if _, err := ex.Dispatch(context.Background(), GitStatusTool, `{"cwd":"/"}`); err == nil {
		t.Fatal("status accepted a caller-controlled directory")
	}
	ex.UntrustedWorkspace = true
	if _, err := ex.Dispatch(context.Background(), GitStatusTool, `{}`); err == nil || !strings.Contains(err.Error(), "trusted workspace") {
		t.Fatalf("status accepted an untrusted workspace: %v", err)
	}
}

func TestGitStatusIgnoresConfiguredWorkTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root, outside := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "-b", "fixture-branch"}, {"config", "core.worktree", outside}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	for dir, name := range map[string]string{root: "inside-marker", outside: "outside-marker"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := (&Executor{Root: root}).Dispatch(context.Background(), GitStatusTool, `{}`)
	if err != nil || !strings.Contains(res.Text, "?? inside-marker") || strings.Contains(res.Text, "outside-marker") {
		t.Fatalf("scoped status = %q, %v", res.Text, err)
	}
}

func TestGitStatusOutputCapAppliesToReaderCopy(t *testing.T) {
	var output gitStatusOutput
	input := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", MaxOutBytes+200))}
	if n, err := io.Copy(&output, input); err != nil || n != MaxOutBytes+200 {
		t.Fatalf("status pipe copy = %d, %v", n, err)
	}
	if !output.truncated || len(output.String()) != MaxOutBytes {
		t.Fatalf("output cap bypassed: bytes=%d truncated=%t", len(output.String()), output.truncated)
	}
}
