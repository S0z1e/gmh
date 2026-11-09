package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultMaxMatches is the default cap on reported matches.
	DefaultMaxMatches = 50
	// MaxMatchesLimit is the hard cap a caller can request.
	MaxMatchesLimit = 200
	// MaxGrepBytes bounds how much search output is read.
	MaxGrepBytes = 2 << 20
)

type GrepArgs struct {
	Pattern    string `json:"pattern"`              // regex, required
	Path       string `json:"path,omitempty"`       // search root, default "."
	Include    string `json:"include,omitempty"`    // glob, e.g. "*.go"
	Exclude    string `json:"exclude,omitempty"`    // glob to exclude
	Context    int    `json:"context,omitempty"`    // lines around each match, default 0, max 5
	MaxMatches int    `json:"maxMatches,omitempty"` // default 50, max 200
}

type grepHit struct {
	path string
	line int
	text string
	ctx  bool
}

// Grep searches for Pattern under Path using ripgrep when available,
// falling back to grep -r with a warning when rg is missing.
// Results are capped; truncation is always reported, never silent.
func (e *Executor) Grep(ctx context.Context, a GrepArgs) (string, error) {
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("grep: empty pattern")
	}
	root := a.Path
	if root == "" {
		root = "."
	}
	dir, err := e.resolve(root)
	if err != nil {
		return "", err
	}
	maxMatches := a.MaxMatches
	if maxMatches <= 0 {
		maxMatches = DefaultMaxMatches
	}
	if maxMatches > MaxMatchesLimit {
		maxMatches = MaxMatchesLimit
	}
	ctxLines := a.Context
	if ctxLines < 0 {
		ctxLines = 0
	}
	if ctxLines > 5 {
		ctxLines = 5
	}

	if _, err := exec.LookPath("rg"); err == nil {
		return e.grepRg(ctx, dir, a, maxMatches, ctxLines)
	}
	out, err := e.grepFallback(ctx, dir, a, maxMatches, ctxLines)
	return "warning: ripgrep (rg) not found, fell back to grep -r\n" + out, err
}

func (e *Executor) grepRg(ctx context.Context, dir string, a GrepArgs, maxMatches, ctxLines int) (string, error) {
	args := []string{"--json", "--line-number", "--no-heading"}
	if ctxLines > 0 {
		args = append(args, "-C", fmt.Sprint(ctxLines))
	}
	if a.Include != "" {
		args = append(args, "-g", a.Include)
	}
	if a.Exclude != "" {
		args = append(args, "-g", "!"+a.Exclude)
	}
	// rg respects .gitignore by default and skips hidden files.
	args = append(args, "--", a.Pattern, dir)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = e.Root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var hits []grepHit
	total := 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	readBytes := 0
	for scanner.Scan() {
		line := scanner.Text()
		readBytes += len(line) + 1
		if readBytes > MaxGrepBytes {
			break
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
				Lines      struct {
					Text string `json:"text"`
				} `json:"lines"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "match" && ev.Type != "context" {
			continue
		}
		if ev.Type == "match" {
			total++
			if total > maxMatches {
				continue // keep counting, stop collecting
			}
		} else if total > maxMatches {
			continue // context belonging to a match past the cap
		}
		rel, rerr := filepath.Rel(e.Root, ev.Data.Path.Text)
		if rerr != nil {
			rel = ev.Data.Path.Text
		}
		hits = append(hits, grepHit{
			path: rel,
			line: ev.Data.LineNumber,
			text: strings.TrimRight(ev.Data.Lines.Text, "\n"),
			ctx:  ev.Type == "context",
		})
	}
	// rg exits 1 when there are no matches; that is not an error here.
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && total == 0 {
			return "no matches for " + a.Pattern, nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("grep timed out after 30s")
		}
		// Any other exit is still reported, with whatever was found.
		if total == 0 {
			return "", fmt.Errorf("rg failed: %w", err)
		}
	}
	if total == 0 {
		return "no matches for " + a.Pattern, nil
	}
	return formatGrepHits(hits, total, maxMatches), nil
}

func (e *Executor) grepFallback(ctx context.Context, dir string, a GrepArgs, maxMatches, ctxLines int) (string, error) {
	args := []string{"-rn", "--exclude-dir=.git", "--exclude-dir=node_modules"}
	if ctxLines > 0 {
		args = append(args, "-C", fmt.Sprint(ctxLines))
	}
	if a.Include != "" {
		args = append(args, "--include="+a.Include)
	}
	args = append(args, "--", a.Pattern, dir)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "grep", args...)
	cmd.Dir = e.Root
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "no matches for " + a.Pattern, nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("grep timed out after 30s")
		}
		return "", fmt.Errorf("grep failed: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	total := len(lines)
	if total == 1 && lines[0] == "" {
		return "no matches for " + a.Pattern, nil
	}
	var hits []grepHit
	for _, l := range lines {
		// grep -rn output: path:line:text (context lines use - as separator)
		parts := strings.SplitN(l, ":", 3)
		if len(parts) < 3 {
			continue
		}
		rel, rerr := filepath.Rel(e.Root, parts[0])
		if rerr != nil {
			rel = parts[0]
		}
		lineNo := 0
		fmt.Sscanf(parts[1], "%d", &lineNo)
		hits = append(hits, grepHit{path: rel, line: lineNo, text: parts[2]})
	}
	if len(hits) > maxMatches {
		hits = hits[:maxMatches]
	}
	return formatGrepHits(hits, total, maxMatches), nil
}

func formatGrepHits(hits []grepHit, total, maxMatches int) string {
	var b strings.Builder
	for _, h := range hits {
		if h.ctx {
			fmt.Fprintf(&b, "%s:%d- %s\n", h.path, h.line, h.text)
		} else {
			fmt.Fprintf(&b, "%s:%d: %s\n", h.path, h.line, h.text)
		}
	}
	if total > maxMatches {
		fmt.Fprintf(&b, "... [truncated: %d matches shown, %d total]\n", maxMatches, total)
	}
	return strings.TrimRight(b.String(), "\n")
}
