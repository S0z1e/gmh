package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// The count is a lower bound when the text marks an incomplete search.
func (e *Executor) Grep(ctx context.Context, a GrepArgs) (string, int, error) {
	r, err := e.grepResult(ctx, a)
	return r.Text, r.Count, err
}

func (e *Executor) grepResult(ctx context.Context, a GrepArgs) (ToolResult, error) {
	if strings.TrimSpace(a.Pattern) == "" {
		return ToolResult{}, fmt.Errorf("grep: empty pattern")
	}
	root := a.Path
	if root == "" {
		root = "."
	}
	dir, err := e.resolve(root)
	if err != nil {
		return ToolResult{}, err
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
	out.Text = "warning: ripgrep (rg) not found, fell back to grep -r\n" + out.Text
	return out, err
}

func (e *Executor) grepRg(ctx context.Context, dir string, a GrepArgs, maxMatches, ctxLines int) (ToolResult, error) {
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

	matches := grepMatches{root: e.Root, max: maxMatches}
	truncated, err := e.streamGrep(ctx, "rg", args, func(line string) error {
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
			return fmt.Errorf("invalid rg output: %w", err)
		}
		if ev.Type != "match" && ev.Type != "context" {
			return nil
		}
		matches.add(ev.Data.Path.Text, ev.Data.LineNumber, strings.TrimRight(ev.Data.Lines.Text, "\n"), ev.Type == "context")
		return nil
	})
	return matches.result(a.Pattern, truncated, err != nil), err
}

func (e *Executor) grepFallback(ctx context.Context, dir string, a GrepArgs, maxMatches, ctxLines int) (ToolResult, error) {
	args := []string{"-rn", "--exclude-dir=.git", "--exclude-dir=node_modules"}
	if ctxLines > 0 {
		args = append(args, "-C", fmt.Sprint(ctxLines))
	}
	if a.Include != "" {
		args = append(args, "--include="+a.Include)
	}
	if a.Exclude != "" {
		args = append(args, "--exclude="+a.Exclude)
	}
	args = append(args, "--", a.Pattern, dir)

	matches := grepMatches{root: e.Root, max: maxMatches}
	truncated, err := e.streamGrep(ctx, "grep", args, func(l string) error {
		if l == "--" {
			return nil
		}
		path, lineNo, text, isCtx, ok := parseGrepFallbackLine(l)
		if !ok {
			return fmt.Errorf("invalid grep output")
		}
		matches.add(path, lineNo, text, isCtx)
		return nil
	})
	return matches.result(a.Pattern, truncated, err != nil), err
}

// streamGrep reads at most MaxGrepBytes+1 bytes, including an oversized
// single record. On a cap or parsing/read error, stop and reap the producer
// before returning instead of waiting with its stdout pipe left full.
func (e *Executor) streamGrep(ctx context.Context, binary string, args []string, consume func(string) error) (bool, error) {
	searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	processCtx, stop := context.WithCancel(searchCtx)
	defer stop()
	cmd := exec.CommandContext(processCtx, binary, args...)
	cmd.Dir = e.Root
	configureProcess(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd) }
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	reader := bufio.NewReader(io.LimitReader(stdout, MaxGrepBytes+1))
	readBytes := 0
	truncated := false
	var readErr error
	for {
		line, err := reader.ReadString('\n')
		readBytes += len(line)
		if readBytes > MaxGrepBytes {
			truncated = true
			break
		}
		if len(line) > 0 {
			if readErr = consume(strings.TrimSuffix(line, "\n")); readErr != nil {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	if truncated || readErr != nil {
		stop()
		_ = stdout.Close()
	}
	waitErr := cmd.Wait()
	if err := searchCtx.Err(); err != nil {
		return truncated, fmt.Errorf("grep: %w", err)
	}
	if readErr != nil {
		return truncated, readErr
	}
	if truncated {
		return true, nil
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		waitErr = nil
	}
	if waitErr != nil {
		return false, fmt.Errorf("%s failed: %w", binary, waitErr)
	}
	return false, nil
}

type grepMatches struct {
	root  string
	max   int
	hits  []grepHit
	total int
}

func (m *grepMatches) add(path string, line int, text string, context bool) {
	if !context {
		m.total++
	}
	if m.total > m.max {
		return
	}
	rel, err := filepath.Rel(m.root, path)
	if err != nil {
		rel = path
	}
	m.hits = append(m.hits, grepHit{path: rel, line: line, text: text, ctx: context})
}

func (m *grepMatches) result(pattern string, outputLimit, failed bool) ToolResult {
	incomplete := outputLimit || failed
	text := formatGrepHits(m.hits, m.total, m.max, incomplete)
	if incomplete {
		reason := "search incomplete"
		if outputLimit {
			reason = "search output limit reached"
		}
		if text != "" {
			text += "\n"
		}
		text += fmt.Sprintf("... [truncated: %d matches shown, at least %d matches; %s]", min(m.total, m.max), m.total, reason)
	} else if m.total == 0 {
		text = "no matches for " + pattern
	}
	return ToolResult{Text: text, Count: m.total, Truncated: incomplete || m.total > m.max, CountLowerBound: incomplete}
}

// parseGrepFallbackLine parses grep -rn output ("path:line:text" or context "path-line-text").
func parseGrepFallbackLine(l string) (path string, lineNo int, text string, isCtx bool, ok bool) {
	if parts := strings.SplitN(l, ":", 3); len(parts) == 3 {
		var n int
		if _, err := fmt.Sscanf(parts[1], "%d", &n); err == nil && n > 0 && fmt.Sprint(n) == parts[1] {
			return parts[0], n, parts[2], false, true
		}
	}
	for i := 0; i < len(l); i++ {
		if l[i] != '-' {
			continue
		}
		rest := l[i+1:]
		dash := strings.IndexByte(rest, '-')
		if dash <= 0 {
			continue
		}
		numStr := rest[:dash]
		var n int
		if _, err := fmt.Sscanf(numStr, "%d", &n); err == nil && n > 0 && fmt.Sprint(n) == numStr {
			return l[:i], n, rest[dash+1:], true, true
		}
	}
	return "", 0, "", false, false
}

func formatGrepHits(hits []grepHit, total, maxMatches int, incomplete bool) string {
	var b strings.Builder
	for _, h := range hits {
		if h.ctx {
			fmt.Fprintf(&b, "%s:%d- %s\n", h.path, h.line, h.text)
		} else {
			fmt.Fprintf(&b, "%s:%d: %s\n", h.path, h.line, h.text)
		}
	}
	if !incomplete && total > maxMatches {
		fmt.Fprintf(&b, "... [truncated: %d matches shown, %d total]\n", maxMatches, total)
	}
	return strings.TrimRight(b.String(), "\n")
}
