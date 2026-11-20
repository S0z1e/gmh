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
	"runtime"
	"slices"
	"strconv"
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
	selected, err := filepath.Abs(e.Root)
	if err != nil {
		return ToolResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	git, err := exec.LookPath("git")
	if err != nil {
		return ToolResult{}, fmt.Errorf("Git status requires git on PATH: %w", err)
	}
	shadow, err := os.MkdirTemp("", "gmh-git-status-*")
	if err != nil {
		return ToolResult{}, err
	}
	defer os.RemoveAll(shadow)
	base := gitStatusCommand{
		ctx: ctx, git: git, dir: selected,
		env: append(bashEnvironment([]string{"PATH", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP"}),
			"HOME="+shadow, "XDG_CONFIG_HOME="+shadow,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "LC_ALL=C"),
	}
	version, err := base.value("version")
	if err != nil {
		return ToolResult{}, fmt.Errorf("determine Git version: %w", err)
	}
	// Override configured worktree selection during discovery. The discovered
	// Git directory, matched to an ancestor's actual .git marker, sets the root.
	discovery := base.withEnv("GIT_WORK_TREE=" + selected)
	gitDir, err := discovery.value("rev-parse", "--absolute-git-dir")
	if err != nil {
		return ToolResult{}, fmt.Errorf("discover Git directory: %w", err)
	}
	if !filepath.IsAbs(gitDir) {
		return ToolResult{}, errInvalidGitMetadata
	}
	root, err := discovery.workTree(gitDir)
	if err != nil {
		return ToolResult{}, err
	}
	checkout := base.withEnv("GIT_WORK_TREE=" + root)
	checkout.dir = root
	repo := checkout.withEnv("GIT_DIR=" + gitDir)
	format, err := repo.value("rev-parse", "--show-object-format")
	if err != nil {
		return ToolResult{}, fmt.Errorf("determine Git object format: %w", err)
	}
	ref, refErr := repo.value("symbolic-ref", "--quiet", "HEAD")
	oid, commitErr := repo.value("rev-parse", "--verify", "--quiet", "HEAD")
	if refErr != nil && (!gitExitOne(refErr) || commitErr != nil) || commitErr != nil && !gitExitOne(commitErr) {
		return ToolResult{}, fmt.Errorf("could not resolve the current Git branch or commit")
	}
	if refErr == nil && (!strings.HasPrefix(ref, "refs/heads/") || !filepath.IsLocal(filepath.FromSlash(ref))) {
		return ToolResult{}, fmt.Errorf("invalid Git branch reference")
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = false\n"
	width := 40
	switch format {
	case "sha1", "--show-object-format": // Older Git echoes the unknown flag; it only supports SHA-1.
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
	if !gitCanScanWorkTree(version) {
		// Git before 2.32 follows symlinked worktree .gitignore and
		// .gitattributes files. Retain HEAD lookup without opening the tree.
		var head string
		switch {
		case ref == "":
			head = "HEAD (detached at " + oid + ")"
		case oid == "":
			head = "No commits yet on " + strings.TrimPrefix(ref, "refs/heads/")
		default:
			head = strings.TrimPrefix(ref, "refs/heads/")
		}
		return ToolResult{Text: "## " + head + "\n\n[Git working tree status omitted; requires a recognized Git version 2.32 or later.]"}, nil
	}
	// These plumbing commands inspect metadata without comparing worktree
	// files. Each path uses its own response because it may contain newlines.
	objects, err := repo.gitPath("objects")
	if err != nil {
		return ToolResult{}, err
	}
	index, err := repo.gitPath("index")
	if err != nil {
		return ToolResult{}, err
	}
	excludes, err := repo.gitPath("info/exclude")
	if err != nil {
		return ToolResult{}, err
	}
	// Let Git use the original discovery path for includeIf.gitdir rules;
	// an explicit canonical GIT_DIR would hide an alias such as .git.
	// Only the validated built-in settings are used by isolated status.
	settings, err := checkout.safeSettings()
	if err != nil {
		return ToolResult{}, err
	}
	config += settings
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
	isolated := base.withEnv("GIT_WORK_TREE="+root, "GIT_DIR="+shadow,
		"GIT_OBJECT_DIRECTORY="+objects, "GIT_INDEX_FILE="+index)
	out, diagnostics, err := isolated.run("-c", "core.excludesFile="+excludes, "status", "--porcelain=v1", "--branch", "--ignore-submodules=dirty", "--", ".")
	if err != nil {
		return ToolResult{Text: out.String(), Truncated: out.truncated}, fmt.Errorf("Git status: %w", err)
	}
	if diagnostics.buffer.Len() != 0 {
		_, _ = out.Write([]byte("\n[Git diagnostics]\n"))
		_, _ = out.Write(diagnostics.buffer.Bytes())
		out.truncated = out.truncated || diagnostics.truncated
	}
	text := out.String()
	if out.truncated {
		text += "\n[additional Git status output omitted]"
	}
	text += "\n[Status does not inspect submodule working trees or run Git filters; upstream tracking is omitted.]"
	return ToolResult{Text: text, Truncated: out.truncated}, nil
}

// Git 2.32 started ignoring symlinked worktree ignore and attribute files.
// Accept released upstream/Apple and Git for Windows version formats. Fall
// back to HEAD-only state for older or unrecognized versions.
func gitCanScanWorkTree(version string) bool {
	fields := strings.Fields(version)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) == 5 && parts[3] == "windows" {
		if _, err := strconv.ParseUint(parts[4], 10, 32); err != nil {
			return false
		}
		parts = parts[:3]
	}
	if len(parts) != 3 {
		return false
	}
	major, majorErr := strconv.ParseUint(parts[0], 10, 32)
	minor, minorErr := strconv.ParseUint(parts[1], 10, 32)
	_, patchErr := strconv.ParseUint(parts[2], 10, 32)
	return majorErr == nil && minorErr == nil && patchErr == nil && (major > 2 || major == 2 && minor >= 32)
}

type gitStatusCommand struct {
	ctx context.Context
	git string
	dir string
	env []string
}

func (g gitStatusCommand) withEnv(extra ...string) gitStatusCommand {
	g.env = slices.Concat(g.env, extra)
	return g
}

func (g gitStatusCommand) run(args ...string) (gitStatusOutput, gitStatusOutput, error) {
	cmd := exec.CommandContext(g.ctx, g.git, append([]string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor="}, args...)...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = g.dir, g.env, time.Second
	var out, diagnostics gitStatusOutput
	cmd.Stdout, cmd.Stderr = &out, &diagnostics
	err := cmd.Run()
	if g.ctx.Err() != nil {
		return out, diagnostics, g.ctx.Err()
	}
	if err != nil && diagnostics.buffer.Len() != 0 {
		text := strings.TrimSpace(diagnostics.String())
		if diagnostics.truncated {
			text += " [additional Git diagnostics omitted]"
		}
		err = fmt.Errorf("%w: %s", err, text)
	}
	return out, diagnostics, err
}

func (g gitStatusCommand) value(args ...string) (string, error) {
	out, _, err := g.run(args...)
	if err != nil {
		return "", err
	}
	value, ok := strings.CutSuffix(out.String(), "\n")
	if !ok || out.truncated || value == "" {
		return "", errInvalidGitMetadata
	}
	if runtime.GOOS == "windows" {
		value = strings.TrimSuffix(value, "\r")
	}
	return value, nil
}

func (g gitStatusCommand) gitPath(name string) (string, error) {
	// --git-path predates --path-format. Resolve relative replies against the
	// explicit checkout root used for this command, not the caller's cwd.
	path, err := g.value("rev-parse", "--git-path", name)
	if err != nil {
		return "", fmt.Errorf("determine Git path %s: %w", name, err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(g.dir, filepath.FromSlash(path))
	}
	return path, nil
}

func (g gitStatusCommand) workTree(gitDir string) (string, error) {
	discovered, err := os.Stat(gitDir)
	if err != nil {
		return "", err
	}
	for dir := g.dir; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".git")
		info, err := os.Stat(marker)
		if err == nil {
			if info.Mode().IsRegular() {
				// Gitfiles (linked worktrees and separate Git directories)
				// can name metadata outside the checkout.
				resolved, resolveErr := g.value("rev-parse", "--resolve-git-dir", marker)
				if resolveErr != nil {
					return "", fmt.Errorf("resolve Git worktree marker: %w", resolveErr)
				}
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(g.dir, filepath.FromSlash(resolved))
				}
				info, err = os.Stat(resolved)
				if err != nil {
					return "", err
				}
			}
			if os.SameFile(info, discovered) {
				return dir, nil
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("workspace is not inside the discovered Git working tree")
		}
	}
}

func (g gitStatusCommand) safeSettings() (string, error) {
	// Only built-in filesystem and text normalization settings may enter the
	// private config. Parse NUL-delimited records before validating values.
	settings, _, err := g.run("config", "--includes", "--null", "--type=bool-or-str", "--get-regexp",
		`^core\.(filemode|ignorecase|symlinks|trustctime|precomposeunicode|autocrlf|eol)$`)
	if err != nil && !gitExitOne(err) || settings.truncated {
		return "", fmt.Errorf("could not read Git filesystem settings")
	}
	var config strings.Builder
	config.WriteString("[core]\n")
	for record := range strings.SplitSeq(settings.String(), "\x00") {
		key, value, ok := strings.Cut(record, "\n")
		if !ok {
			continue
		}
		value = strings.ToLower(value)
		valid := false
		switch key {
		case "core.filemode", "core.ignorecase", "core.symlinks", "core.trustctime", "core.precomposeunicode":
			valid = value == "true" || value == "false"
		case "core.autocrlf":
			valid = value == "true" || value == "false" || value == "input"
		case "core.eol":
			valid = value == "lf" || value == "crlf" || value == "native"
		}
		if valid {
			fmt.Fprintf(&config, "\t%s = %s\n", strings.TrimPrefix(key, "core."), value)
		}
	}
	return config.String(), nil
}

var errInvalidGitMetadata = errors.New("invalid or truncated Git metadata")

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
