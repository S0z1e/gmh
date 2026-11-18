package tools

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestGitStatusPreservesBuiltInNormalization(t *testing.T) {
	root, git := gitStatusTestRepo(t)
	git("config", "core.autocrlf", "true")
	tracked := filepath.Join(root, "windows-lines.txt")
	if err := os.WriteFile(tracked, []byte("line\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "windows-lines.txt")
	git("commit", "-m", "fixture")
	// Force a content comparison without changing the file. CRLF is the
	// clean representation selected by this checkout for the LF blob.
	mtime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tracked, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if out := git("--no-optional-locks", "status", "--porcelain=v1"); out != "" {
		t.Fatalf("fixture is not clean according to Git: %q", out)
	}
	res, err := (&Executor{Root: root}).Dispatch(context.Background(), GitStatusTool, `{}`)
	if err != nil || strings.Contains(res.Text, "windows-lines.txt") || !strings.HasPrefix(res.Text, "## fixture-branch") {
		t.Fatalf("status changed the repository's built-in normalization: %q, %v", res.Text, err)
	}
}

func TestGitStatusPreservesMacOSFilenameNormalization(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("precomposeUnicode applies to macOS")
	}
	root, git := gitStatusTestRepo(t)
	git("config", "core.precomposeunicode", "true")
	const name = "cafe\u0301.txt"
	if err := os.WriteFile(filepath.Join(root, name), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", name)
	git("commit", "-m", "fixture")
	if err := os.WriteFile(filepath.Join(root, name), []byte("after normalization"), 0600); err != nil {
		t.Fatal(err)
	}
	want := git("--no-optional-locks", "status", "--porcelain=v1")
	without := git("--no-optional-locks", "-c", "core.precomposeunicode=false", "status", "--porcelain=v1")
	if want == without {
		t.Skip("filesystem and Git do not distinguish the fixture's normalization")
	}
	res, err := (&Executor{Root: root}).Dispatch(context.Background(), GitStatusTool, `{}`)
	if err != nil || !strings.Contains(res.Text, "\n"+want+"\n") {
		t.Fatalf("status did not match Git's filename normalization: %q, %v; want %q", res.Text, err, want)
	}
}

func TestGitStatusUsesRepositoryDiscoveredByGit(t *testing.T) {
	t.Run("empty nested marker", func(t *testing.T) {
		root, git := gitStatusTestRepo(t)
		sub := filepath.Join(root, "nested")
		if err := os.MkdirAll(filepath.Join(sub, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "tracked.txt"), []byte("tracked"), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "nested/tracked.txt")
		git("commit", "-m", "fixture")
		if err := os.WriteFile(filepath.Join(root, "outside.txt"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		res, err := (&Executor{Root: sub}).Dispatch(context.Background(), GitStatusTool, `{}`)
		if err != nil || !strings.HasPrefix(res.Text, "## fixture-branch") || strings.Contains(res.Text, ".txt") {
			t.Fatalf("nested status used the wrong tree or scope: %q, %v", res.Text, err)
		}
	})
	t.Run("symlinked metadata", func(t *testing.T) {
		root, git := gitStatusTestRepo(t)
		metadata := filepath.Join(filepath.Dir(root), "external-git-dir")
		marker := filepath.Join(root, ".git")
		if err := os.Rename(marker, metadata); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(metadata, marker); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("symlinks unavailable: %v", err)
			}
			t.Fatal(err)
		}
		git("status", "--short")
		res, err := (&Executor{Root: root}).Dispatch(context.Background(), GitStatusTool, `{}`)
		if err != nil || !strings.Contains(res.Text, "fixture-branch") {
			t.Fatalf("status rejected a Git directory accepted by Git: %q, %v", res.Text, err)
		}
	})
}

func TestGitStatusDoesNotParseGitDiagnostics(t *testing.T) {
	root, git := gitStatusTestRepo(t)
	git("config", "core.fsync", "fixture-unknown-component")
	if !strings.Contains(git("rev-parse", "--git-path", "index"), "fixture-unknown-component") {
		t.Skip("installed Git does not emit the fixture warning")
	}
	res, err := (&Executor{Root: root}).Dispatch(context.Background(), GitStatusTool, `{}`)
	if err != nil || !strings.Contains(res.Text, "fixture-branch") || strings.Contains(res.Text, "fixture-unknown-component") {
		t.Fatalf("status treated a diagnostic as metadata: %q, %v", res.Text, err)
	}
}

func TestGitStatusPreservesGitNames(t *testing.T) {
	root, git := gitStatusTestRepo(t)
	const branch = "name\u00a0"
	git("symbolic-ref", "HEAD", "refs/heads/"+branch)
	t.Run("branch whitespace", func(t *testing.T) {
		res, err := (&Executor{Root: root}).Dispatch(context.Background(), GitStatusTool, `{}`)
		if err != nil || !strings.Contains(strings.ReplaceAll(res.Text, "\r\n", "\n"), branch+"\n") {
			t.Fatalf("status changed a valid branch name: %q, %v", res.Text, err)
		}
	})
	t.Run("metadata path newline", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows paths cannot contain newlines")
		}
		newRoot := filepath.Join(filepath.Dir(root), "with\nnewline\r")
		if err := os.Rename(root, newRoot); err != nil {
			t.Fatal(err)
		}
		res, err := (&Executor{Root: newRoot}).Dispatch(context.Background(), GitStatusTool, `{}`)
		if err != nil || !strings.Contains(strings.ReplaceAll(res.Text, "\r\n", "\n"), branch+"\n") {
			t.Fatalf("status lost a valid metadata path: %q, %v", res.Text, err)
		}
	})
}

func gitStatusTestRepo(t *testing.T) (string, func(...string) string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gitPath, append([]string{"-c", "user.name=Test", "-c", "user.email=fixture@example.test"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	git("init", "-b", "fixture-branch")
	return root, git
}
