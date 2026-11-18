package tools

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"minimalharness/internal/workspace"
)

const GitStatusTool = "git_status"

// gitStatus has a fixed workspace and command. A private Git directory
// supplies configuration and HEAD; the real index and objects are read-only
// inputs. In particular, status never loads executable repository filters.
func (e *Executor) gitStatus(ctx context.Context) (ToolResult, error) {
	if e.UntrustedWorkspace {
		return ToolResult{}, &PolicyError{Reason: "Git status is unavailable without a trusted workspace"}
	}
	if e.Guidance != nil {
		identity, err := workspace.Identify(e.Root)
		if err != nil || identity.FileID != e.Guidance.FileID() {
			return ToolResult{}, &PolicyError{Reason: "workspace directory identity changed"}
		}
	}
	root, err := gitWorkTree(e.Root)
	if err != nil {
		return ToolResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	git, err := exec.LookPath("git")
	if err != nil {
		return ToolResult{}, fmt.Errorf("Git status requires git on PATH: %w", err)
	}
	env := append(bashEnvironment([]string{"PATH", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP"}),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "LC_ALL=C",
		"GIT_WORK_TREE="+root)
	run := func(env []string, args ...string) (gitStatusOutput, error) {
		cmd := exec.CommandContext(ctx, git, append([]string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor="}, args...)...)
		cmd.Dir = e.Root
		cmd.Env = env
		cmd.WaitDelay = time.Second
		var output gitStatusOutput
		cmd.Stdout, cmd.Stderr = &output, &output
		err := cmd.Run()
		if ctx.Err() != nil {
			return output, ctx.Err()
		}
		return output, err
	}
	// These plumbing calls inspect metadata without comparing worktree files.
	meta, err := run(env, "rev-parse", "--path-format=absolute", "--git-path", "objects", "--git-path", "index", "--git-path", "info/exclude", "--show-object-format")
	if err != nil {
		return ToolResult{Text: meta.String()}, fmt.Errorf("inspect Git worktree metadata: %w", err)
	}
	parts := strings.Split(strings.TrimRight(meta.String(), "\r\n"), "\n")
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], "\r")
	}
	if meta.truncated || len(parts) != 4 || !filepath.IsAbs(parts[0]) || !filepath.IsAbs(parts[1]) || !filepath.IsAbs(parts[2]) {
		return ToolResult{}, fmt.Errorf("could not determine Git metadata paths for this workspace")
	}
	objects, index, excludes, format := parts[0], parts[1], parts[2], parts[3]
	head, refErr := run(env, "symbolic-ref", "--quiet", "HEAD")
	commit, commitErr := run(env, "rev-parse", "--verify", "--quiet", "HEAD")
	ref, oid := strings.TrimSpace(head.String()), strings.TrimSpace(commit.String())
	if refErr != nil && (!gitExitOne(refErr) || commitErr != nil) || commitErr != nil && !gitExitOne(commitErr) {
		return ToolResult{}, fmt.Errorf("could not resolve the current Git branch or commit")
	}
	if refErr == nil && (!strings.HasPrefix(ref, "refs/heads/") || !filepath.IsLocal(filepath.FromSlash(ref))) {
		return ToolResult{}, fmt.Errorf("invalid Git branch reference")
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = false\n"
	width := 40
	switch format {
	case "sha1":
	case "sha256":
		config = "[core]\n\trepositoryformatversion = 1\n\tbare = false\n[extensions]\n\tobjectformat = sha256\n"
		width = 64
	default:
		return ToolResult{}, fmt.Errorf("unsupported Git object format")
	}
	if commitErr == nil {
		if _, err := hex.DecodeString(oid); err != nil || len(oid) != width {
			return ToolResult{}, fmt.Errorf("invalid Git commit identity")
		}
	}
	// Preserve safe filesystem settings without copying repository config or
	// giving its includes, filter commands or worktree setting to status.
	settings, err := run(env, "config", "--includes", "--type=bool", "--get-regexp", `^core\.(filemode|ignorecase|symlinks|trustctime)$`)
	if err != nil && !gitExitOne(err) || settings.truncated {
		return ToolResult{}, fmt.Errorf("could not read Git filesystem settings")
	}
	config += "[core]\n"
	for line := range strings.SplitSeq(settings.String(), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		switch key {
		case "core.filemode", "core.ignorecase", "core.symlinks", "core.trustctime":
			if ok && (value == "true" || value == "false") {
				config += "\t" + strings.TrimPrefix(key, "core.") + " = " + value + "\n"
			}
		}
	}
	shadow, err := os.MkdirTemp("", "gmh-git-status-*")
	if err != nil {
		return ToolResult{}, err
	}
	defer os.RemoveAll(shadow)
	for _, dir := range []string{"refs/heads", "objects", "info"} {
		if err := os.MkdirAll(filepath.Join(shadow, dir), 0700); err != nil {
			return ToolResult{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(shadow, "config"), []byte(config), 0600); err != nil {
		return ToolResult{}, err
	}
	headText := oid
	if refErr == nil {
		headText = "ref: " + ref
		if commitErr == nil {
			path := filepath.Join(shadow, filepath.FromSlash(ref))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return ToolResult{}, err
			}
			if err := os.WriteFile(path, []byte(oid+"\n"), 0600); err != nil {
				return ToolResult{}, err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(shadow, "HEAD"), []byte(headText+"\n"), 0600); err != nil {
		return ToolResult{}, err
	}
	env = append(env, "GIT_DIR="+shadow, "GIT_OBJECT_DIRECTORY="+objects, "GIT_INDEX_FILE="+index)
	out, err := run(env, "-c", "core.excludesFile="+excludes, "status", "--porcelain=v1", "--branch", "--ignore-submodules=dirty", "--", ".")
	text := out.String()
	if err != nil {
		return ToolResult{Text: text, Truncated: out.truncated}, fmt.Errorf("Git status: %w", err)
	}
	if out.truncated {
		text += "\n[additional Git status output omitted]"
	}
	text += "\n[Status does not inspect submodule working trees or run Git filters; upstream tracking is omitted.]"
	return ToolResult{Text: text, Truncated: out.truncated}, nil
}

// The filesystem marker, rather than core.worktree, selects the working tree.
func gitWorkTree(dir string) (string, error) {
	for {
		info, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			if !info.IsDir() && !info.Mode().IsRegular() {
				return "", fmt.Errorf("unsupported Git worktree marker")
			}
			return dir, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("workspace is not inside a Git working tree")
		}
		dir = parent
	}
}

func gitExitOne(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

type gitStatusOutput struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *gitStatusOutput) String() string { return b.buffer.String() }

func (b *gitStatusOutput) Write(p []byte) (int, error) {
	n := min(len(p), MaxOutBytes-b.buffer.Len())
	_, _ = b.buffer.Write(p[:n])
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}
