package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	maxGitStatusLines = 30
	maxGitLogLines    = 5
	gitTimeout        = 2 * time.Second
)

func ProjectGitContext(root string) string {
	if snap := gitSnapshot(root); snap != "" {
		return "\n\nGit status (as of startup):\n" + snap
	}
	return ""
}

// gitSnapshot returns branch + status + recent log, or "" when the
// root is not a git repository or git fails.
func gitSnapshot(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", err
		}
		return strings.TrimRight(out.String(), "\n"), nil
	}
	branch, err := run("branch", "--show-current")
	if err != nil {
		return ""
	}
	var b strings.Builder
	if branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", branch)
	} else {
		b.WriteString("branch: (detached HEAD)\n")
	}
	if status, err := run("status", "--porcelain=v1", "--branch"); err == nil {
		var lines []string
		for _, l := range strings.Split(status, "\n") {
			if strings.HasPrefix(l, "## ") || l == "" {
				continue
			}
			lines = append(lines, l)
		}
		if len(lines) == 0 {
			b.WriteString("working tree clean\n")
		} else {
			for i, l := range lines {
				if i >= maxGitStatusLines {
					fmt.Fprintf(&b, "(%d more changed files)\n", len(lines)-maxGitStatusLines)
					break
				}
				b.WriteString(l + "\n")
			}
		}
	}
	if log, err := run("log", "--oneline", "-5"); err == nil && log != "" {
		b.WriteString("recent commits:\n")
		for i, l := range strings.Split(log, "\n") {
			if i >= maxGitLogLines {
				break
			}
			b.WriteString("  " + l + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
