// Package tools provides the tools: read, write, edit, bash, grep, glob,
// load_skill. All paths are jailed to the workspace root. bash runs with
// a timeout.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/skills"
)

const (
	MaxReadBytes       = 1 << 20 // 1 MiB per read
	MaxOutBytes        = 64 << 10
	DefaultBashTimeout = 60 * time.Second
	MaxBashTimeout     = 300 * time.Second
	// DefaultBackgroundThreshold is how long a bash call runs before
	// the loop backgrounds it instead of blocking (0 = disabled).
	DefaultBackgroundThreshold = 10 * time.Second
)

type Executor struct {
	Root string
	// BashTimeout overrides the default 60s bash timeout (0 = default).
	// The per-call timeout argument still caps at 300s.
	BashTimeout time.Duration
	// BackgroundThreshold is how long a bash call may run before the
	// loop backgrounds it (0 = never background; DefaultBackgroundThreshold
	// when Setup leaves it unset).
	BackgroundThreshold time.Duration
	// Audit receives one entry per bash execution and denylist denial
	// (nil = no audit). Approval decisions are audited by the loop.
	Audit func(AuditEntry)

	jobsMu sync.Mutex
	jobs   *JobRegistry
}

func (e *Executor) audit(entry AuditEntry) {
	if e.Audit != nil {
		entry.TS = time.Now()
		e.Audit(entry)
	}
}

func (e *Executor) resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(e.Root, clean)
	}
	// Jail: resolved path must stay inside Root.
	rel, err := filepath.Rel(e.Root, clean)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path outside workspace root: %s", p)
	}
	return clean, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n... [truncated]"
	}
	return s
}

type ReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (e *Executor) Read(a ReadArgs) (string, error) {
	p, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(p)
		if err != nil {
			return "", err
		}
		var names []string
		for _, en := range entries {
			n := en.Name()
			if en.IsDir() {
				n += "/"
			}
			names = append(names, n)
		}
		return "directory " + a.Path + ":\n" + strings.Join(names, "\n"), nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	data = []byte(truncate(string(data), MaxReadBytes))
	lines := strings.Split(string(data), "\n")
	start := a.Offset
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return "", nil
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}
	return b.String(), nil
}

type WriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (e *Executor) Write(a WriteArgs) (string, error) {
	p, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err == nil {
		return "", fmt.Errorf("file exists, use edit instead: %s", a.Path)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return "wrote " + a.Path, nil
}

type EditArgs struct {
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

func (e *Executor) Edit(a EditArgs) (string, error) {
	p, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := string(data)
	if n := strings.Count(s, a.OldText); n != 1 {
		return "", fmt.Errorf("oldText must match exactly once, got %d matches in %s", n, a.Path)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(s, a.OldText, a.NewText, 1)), 0o644); err != nil {
		return "", err
	}
	return "edited " + a.Path, nil
}

type BashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 60, max 300
}

// bashTimeout resolves the effective timeout for one bash call:
// per-call argument (capped at 300s), else the executor default.
func (e *Executor) bashTimeout(a BashArgs) time.Duration {
	timeout := DefaultBashTimeout
	if e.BashTimeout > 0 {
		timeout = e.BashTimeout
	}
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
		if timeout > MaxBashTimeout {
			timeout = MaxBashTimeout
		}
	}
	return timeout
}

func (e *Executor) Bash(ctx context.Context, a BashArgs) (string, error) {
	// Denylist first: the refusal is an observation, not a crash, and
	// it is audited like any other denial.
	if reason := DeniedCommand(a.Command); reason != "" {
		e.audit(AuditEntry{Kind: "denied", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: reason})
		return "", fmt.Errorf("denied by policy: %s (this is harm reduction, not a sandbox — see README)", reason)
	}
	timeout := e.bashTimeout(a)
	s, exitCode, err := runBashCmd(ctx, e.Root, a.Command, timeout)
	var te *timeoutError
	switch {
	case errors.As(err, &te):
		e.audit(AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: fmt.Sprintf("timed out after %s", timeout)})
		return s, err
	case err != nil:
		e.audit(AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: exitCode, Error: err.Error()})
		return s, err
	default:
		e.audit(AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: exitCode})
		return s, nil
	}
}

// Dispatch runs a tool call by name and returns the observation string.
func (e *Executor) Dispatch(ctx context.Context, name, argsJSON string) (string, error) {
	switch name {
	case "read":
		var a ReadArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("bad read args: %w", err)
		}
		return e.Read(a)
	case "write":
		var a WriteArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("bad write args: %w", err)
		}
		return e.Write(a)
	case "edit":
		var a EditArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("bad edit args: %w", err)
		}
		return e.Edit(a)
	case "bash":
		var a BashArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("bad bash args: %w", err)
		}
		return e.Bash(ctx, a)
	case "grep":
		var a GrepArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("bad grep args: %w", err)
		}
		return e.Grep(ctx, a)
	case "glob":
		var a GlobArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("bad glob args: %w", err)
		}
		return e.Glob(ctx, a)
	case "load_skill":
		var a LoadSkillArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("bad load_skill args: %w", err)
		}
		return e.LoadSkill(a)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// LoadSkillArgs names one skill to load, and optionally one file from
// its references/ directory.
type LoadSkillArgs struct {
	Name      string `json:"name"`
	Reference string `json:"reference,omitempty"`
}

// LoadSkill returns the SKILL.md body for a skill by name (plus a footer
// listing its references/), or the content of one references/ file when
// Reference is set. Read-only: it never writes, so it needs no approval.
// The name match is exact first, then case-insensitive.
func (e *Executor) LoadSkill(a LoadSkillArgs) (string, error) {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return "", fmt.Errorf("load_skill needs a skill name")
	}
	found := skills.Discover(e.Root)
	var match *skills.Skill
	for i, sk := range found {
		if sk.Name == name {
			match = &found[i]
			break
		}
	}
	if match == nil {
		for i, sk := range found {
			if strings.EqualFold(sk.Name, name) {
				match = &found[i]
				break
			}
		}
	}
	if match == nil {
		var names []string
		for _, sk := range found {
			names = append(names, sk.Name)
		}
		return "", fmt.Errorf("unknown skill %q (available: %s)", name, strings.Join(names, ", "))
	}
	if strings.TrimSpace(a.Reference) != "" {
		return match.LoadReference(a.Reference)
	}
	return match.Load()
}
