package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Project context: the two context-file names the major agents agree
// on (AGENTS.md and CLAUDE.md), plus a git
// snapshot. They are loaded once at Setup and appended to the system
// prompt; editing a file mid-session needs a restart, like the skills
// listing. Each file is capped so a huge context file cannot flood
// the prompt, and the git commands are read-only with a short
// timeout, so a slow or missing git simply drops the snapshot.

const (
	maxContextFileBytes = 8 * 1024
	maxContextLevels    = 8 // root + ancestors, never above $HOME
	maxGitStatusLines   = 30
	maxGitLogLines      = 5
	gitTimeout          = 2 * time.Second
)

var contextFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// ProjectContext renders the project-context section for the system
// prompt: context files from the root upward, then a git snapshot
// (branch, working-tree status, recent commits) as of startup. It
// returns "" when the root has neither. The files carry a one-line
// instruction to use them directly: left bare, a reasoning model
// goes looking for the files with tools instead of trusting the
// prompt (observed live: it explored 22 tool calls, then guessed
// the codename from t.txt when the file was gone).
func ProjectContext(root string) string {
	var files strings.Builder
	for _, f := range findContextFiles(root) {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		text := string(data)
		if len(data) > maxContextFileBytes {
			text = string(data[:maxContextFileBytes]) + "\n[truncated: file continues, first 8 KiB shown]"
		}
		fmt.Fprintf(&files, "\n\nProject context (%s):\n%s", f.display, text)
	}
	var b strings.Builder
	if files.Len() > 0 {
		b.WriteString("\n\nProject context, loaded at startup from the project's own context files. Use the facts and instructions below directly — do not re-read the files to verify them.")
		b.WriteString(files.String())
	}
	if snap := gitSnapshot(root); snap != "" {
		b.WriteString("\n\nGit status (as of startup):\n" + snap)
	}
	return b.String()
}

type contextFile struct {
	path    string
	display string // path relative to the root, or the bare name
}

// findContextFiles collects AGENTS.md/CLAUDE.md from the root upward.
// The root itself is always searched; ancestors are searched only
// while they sit at or below $HOME, so a root in /tmp never pulls in
// context from / or an unrelated tree.
func findContextFiles(root string) []contextFile {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	var out []contextFile
	dir := abs
	for level := 0; level < maxContextLevels; level++ {
		for _, name := range contextFileNames {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				display := name
				if rel, err := filepath.Rel(abs, p); err == nil {
					display = rel
				}
				out = append(out, contextFile{path: p, display: display})
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir || home == "" {
			break
		}
		if parent != home && !strings.HasPrefix(parent, home+string(os.PathSeparator)) {
			break
		}
		dir = parent
	}
	return out
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
