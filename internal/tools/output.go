// Bash output files (output-to-file batch). Every bash execution
// writes its FULL stdout+stderr to a per-call file under the
// executor's OutputDir (session-scoped, set by app.Setup), and the
// observation returned to the model carries only head + tail plus
// the file's path. The file grows while a background job runs, so
// the model can read partial output mid-run with the read tool —
// the pull affordance every surveyed production agent has (Claude
// reads an output file, OpenCode a synthetic task message, Codex
// wait_agent). Without OutputDir the executor falls back to the old
// in-memory behaviour (CombinedOutput capped at MaxOutBytes).
package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// BashOutputHeadLines / BashOutputTailLines are how many lines
	// of a long bash output the model sees inline.
	BashOutputHeadLines = 30
	BashOutputTailLines = 30
	// bashOutputInlineMaxChars bounds the inline result (head+tail
	// included) so one enormous line cannot blow up the context;
	// the full output is always in the file.
	bashOutputInlineMaxChars = 32 * 1024
	// bashOutputMaxLineChars caps one stored head/tail line in the
	// inline result; the file keeps the line whole.
	bashOutputMaxLineChars = 4096
	// OutputFileMaxBytes caps one output file on disk: a command
	// spewing output for the whole 600s timeout must not fill the
	// disk. Past the cap the file stops growing and the inline
	// result says so; the file still holds the first 64 MiB.
	OutputFileMaxBytes = 64 << 20
)

// bashOutputSink keeps the first BashOutputHeadLines lines, the
// last BashOutputTailLines lines, and the line/byte totals for one
// bash output, so the inline result can be formatted without
// re-reading the file. It is fed bytes (usually by scanOutputFile,
// which streams the output file through it) — it is NOT the child's
// stdout: a custom Writer there would force os/exec to build a pipe,
// and cmd.Wait() would block until every grandchild holding that
// pipe closed it (the café deadlock: a dev server started with `&`
// blocked turn end forever). The child writes straight to the file
// (an *os.File), and the accounting is re-derived from the file
// after the child exits. It is safe for concurrent use.
type bashOutputSink struct {
	mu      sync.Mutex
	pending []byte // bytes of the current, unterminated line
	lines   int
	bytes   int64
	head    []string
	tail    []string // ring: last BashOutputTailLines lines
	capped  bool     // fed more than OutputFileMaxBytes
	closed  bool
}

func newBashOutputSink() *bashOutputSink {
	return &bashOutputSink{}
}

// Write feeds p through the line splitter. A line is text
// terminated by '\n'; a trailing partial line is counted when
// Close flushes it.
func (s *bashOutputSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	if !s.capped && s.bytes+int64(len(p)) > OutputFileMaxBytes {
		s.capped = true
	}
	s.bytes += int64(len(p))
	s.pending = append(s.pending, p...)
	for {
		i := indexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		s.addLine(string(s.pending[:i]))
		s.pending = s.pending[i+1:]
	}
	return len(p), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// addLine records one complete line. Callers hold s.mu.
func (s *bashOutputSink) addLine(line string) {
	line = strings.TrimSuffix(line, "\r")
	s.lines++
	if len(s.head) < BashOutputHeadLines {
		s.head = append(s.head, capLine(line))
	}
	s.tail = append(s.tail, capLine(line))
	if len(s.tail) > BashOutputTailLines {
		s.tail = s.tail[1:]
	}
}

// capLine bounds one inline line; the file keeps it whole.
func capLine(line string) string {
	if len(line) <= bashOutputMaxLineChars {
		return line
	}
	return line[:bashOutputMaxLineChars] + "… [line truncated]"
}

// Close flushes a trailing partial line as a line (when non-empty).
// It is safe to call twice.
func (s *bashOutputSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if len(s.pending) > 0 {
		s.addLine(string(s.pending))
		s.pending = nil
	}
	return nil
}

// snapshot returns the totals and the head/tail lines for formatting.
func (s *bashOutputSink) snapshot() (lines int, bytes int64, capped bool, head, tail []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head = append([]string(nil), s.head...)
	tail = append([]string(nil), s.tail...)
	return s.lines, s.bytes, s.capped, head, tail
}

// scanOutputFile re-derives a finished output file's accounting by
// streaming it through a fresh sink: line/byte totals plus head and
// tail. The child writes straight to the file, so the scan runs
// after Wait returns; a grandchild may still be appending (a dev
// server's logs), which is fine — the scan is a point-in-time view
// and the file keeps growing for later reads.
func scanOutputFile(path string) (lines int, bytes int64, capped bool, head, tail []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false, nil, nil, err
	}
	defer f.Close()
	sink := newBashOutputSink()
	buf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(sink, f, buf); err != nil {
		return 0, 0, false, nil, nil, err
	}
	if err := sink.Close(); err != nil {
		return 0, 0, false, nil, nil, err
	}
	lines, bytes, capped, head, tail = sink.snapshot()
	return lines, bytes, capped, head, tail, nil
}

// FormatBashOutput renders the inline result for a finished (or
// timed-out) bash call: a header line with the totals, the output
// file's path and the directory the call ran in, then the whole
// output when it is short, else head + tail with a marker. The
// header comes first so it survives every downstream cap (agent
// capText and RPC capOutput both keep head + tail). The cwd is in
// the header because every call is a fresh shell starting in the
// workspace root: without it the model cannot tell a bare `npm run
// build` ran in the wrong directory.
func FormatBashOutput(lines int, bytes int64, fileCapped bool, head, tail []string, path, cwd string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[bash output: %d lines, %d bytes; full output in %s; cwd: %s]", lines, bytes, path, cwd)
	if fileCapped {
		fmt.Fprintf(&b, " [output file capped at %d bytes]", OutputFileMaxBytes)
	}
	if lines == 0 {
		return b.String()
	}
	b.WriteString("\n")
	if lines <= BashOutputHeadLines+BashOutputTailLines {
		// Short output: whole, from the head lines (they hold all
		// of it). A whole output over the inline char budget is
		// still shown head + tail by the loop below.
		whole := strings.Join(head, "\n")
		if len(whole) <= bashOutputInlineMaxChars {
			b.WriteString(whole)
			return b.String()
		}
	}
	// Long output: first 30 + marker + last 30. Drop tail lines
	// from the front until the inline result fits the char budget
	// (the header, the marker and at least the last line survive).
	marker := fmt.Sprintf("\n... [showing first %d and last %d of %d lines] ...\n", BashOutputHeadLines, BashOutputTailLines, lines)
	for {
		out := strings.Join(head, "\n") + marker + strings.Join(tail, "\n")
		if len(b.String())+len(out) <= bashOutputInlineMaxChars || len(tail) <= 1 {
			b.WriteString(out)
			return b.String()
		}
		tail = tail[1:]
	}
}

// nextOutputFile allocates the path for one bash call's output
// file, creating OutputDir on first use. It returns "" when
// OutputDir is unset or cannot be created: callers then fall back
// to the in-memory behaviour. Names carry the pid so two processes
// resuming the same session never share a file. The file is
// created (empty) here, so the path exists from the moment a
// background job starts — a reader polling it mid-run never races
// the goroutine that fills it.
func (e *Executor) nextOutputFile() string {
	if e.OutputDir == "" {
		return ""
	}
	e.outMu.Lock()
	defer e.outMu.Unlock()
	if err := os.MkdirAll(e.OutputDir, 0o755); err != nil {
		return ""
	}
	e.outSeq++
	path := filepath.Join(e.OutputDir, fmt.Sprintf("bash-%d-%d.log", os.Getpid(), e.outSeq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return ""
	}
	_ = f.Close()
	return path
}

// outputDirBase resolves p for the read tool's narrow exception:
// an absolute path inside OutputDir is readable even though it
// sits outside the workspace root. It returns ("", false) when
// OutputDir is unset or p is not under it. Only Read uses this;
// Write, Edit, Glob and Grep keep the workspace jail, so the model
// can never write into the session directory.
func (e *Executor) outputDirBase(p string) (string, bool) {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		return "", false
	}
	if e.OutputDir != "" {
		rel, err := filepath.Rel(e.OutputDir, clean)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return e.OutputDir, true
		}
	}
	return "", false
}
