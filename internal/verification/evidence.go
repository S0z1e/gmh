// Package verification binds command results to the workspace bytes they checked.
// A model's answer or a successful process without a matching input version is
// not completion evidence.
package verification

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Check struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
}

type Evidence struct {
	InvocationID string    `json:"invocation_id,omitempty"`
	ToolCallID   string    `json:"tool_call_id,omitempty"`
	Check        string    `json:"check"`
	Command      string    `json:"command"`
	Cwd          string    `json:"cwd"`
	Version      string    `json:"version"`
	ExitCode     *int      `json:"exit_code"`
	Started      time.Time `json:"started"`
	Finished     time.Time `json:"finished"`
	Error        string    `json:"error,omitempty"`
}

// Directory is the canonical, in-workspace cwd whose files the version covers.
// Commands may be more powerful than this boundary; this only defines evidence.
func (c Check) Directory(root string) (string, error) {
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Command) == "" {
		return "", fmt.Errorf("verification requires name and command")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	dir := c.Cwd
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("verification cwd is outside the versioned workspace")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("verification cwd must be a directory")
	}
	return dir, nil
}

// Current accepts only a successful, timed execution of this exact command,
// directory and input version. Callers must preserve the latest attempt even
// when it fails; an older pass must not mask a later failed check.
func (e Evidence) Current(c Check, state Snapshot) bool {
	dir, err := c.Directory(state.Root.Path)
	return err == nil && e.Check == c.Name && e.Command == c.Command && e.Cwd == dir &&
		state.Version != "" && e.Version == state.Version && e.ExitCode != nil && *e.ExitCode == 0 && e.Error == "" &&
		!e.Started.IsZero() && !e.Finished.Before(e.Started)
}
