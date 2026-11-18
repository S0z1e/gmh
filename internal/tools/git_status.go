package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"minimalharness/internal/workspace"
)

const GitStatusTool = "git_status"

// gitStatus has a fixed workspace and command. It does not use the Bash
// sandbox: Git can need the common metadata directory of a linked worktree.
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	git, err := exec.LookPath("git")
	if err != nil {
		return ToolResult{}, fmt.Errorf("Git status requires git on PATH: %w", err)
	}
	env := append(bashEnvironment([]string{"PATH", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP"}),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "LC_ALL=C")
	run := func(args ...string) (gitStatusOutput, error) {
		fixed := []string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=", "-c", "status.submoduleSummary=false"}
		cmd := exec.CommandContext(ctx, git, append(fixed, args...)...)
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
	// A clean/process filter could run a command when status compares a
	// changed file with the index. Do not run status with those drivers.
	configured, err := run("config", "--includes", "--name-only", "--get-regexp", `^filter\..*\.(clean|process)$`)
	var exit *exec.ExitError
	if err == nil {
		return ToolResult{}, &PolicyError{Reason: "this repository configures Git filters that can execute commands; use the normal Bash approval path to inspect its status"}
	}
	// Git config's exit status 1 with no output means no matching keys.
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || configured.Len() != 0 {
		return ToolResult{}, fmt.Errorf("check Git configuration: %w", err)
	}
	out, err := run("status", "--porcelain=v1", "--branch", "--ignore-submodules=dirty", "--", ".")
	text := out.String()
	if out.truncated {
		text += "\n[additional Git status output omitted]"
	}
	if err != nil {
		err = fmt.Errorf("Git status: %w", err)
	}
	return ToolResult{Text: text, Truncated: out.truncated}, err
}

type gitStatusOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *gitStatusOutput) Write(p []byte) (int, error) {
	n := min(len(p), MaxOutBytes-b.Len())
	_, _ = b.Buffer.Write(p[:n])
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}
