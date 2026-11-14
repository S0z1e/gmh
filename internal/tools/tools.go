// Package tools provides the tools: read, write, edit, bash, grep, glob,
// load_skill, webfetch. All paths are jailed to the workspace root. bash runs with
// a timeout.
package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/fileguard"
	"minimalharness/internal/llm"
	"minimalharness/internal/skills"
)

const (
	MaxReadBytes       = 1 << 20 // 1 MiB per read
	MaxOutBytes        = 64 << 10
	DefaultBashTimeout = 120 * time.Second
	MaxBashTimeout     = 600 * time.Second
	// DefaultBackgroundThreshold is how long a bash call runs before
	// the loop backgrounds it instead of blocking (0 = disabled).
	// 120s is the industry consensus (Claude Code, OpenCode): a
	// normal install, build or test stays foreground and returns
	// its result directly; only the truly long tail backgrounds.
	DefaultBackgroundThreshold = 120 * time.Second
)

type Executor struct {
	Root      string
	editMu    sync.Mutex
	snapshots map[string][32]byte
	// BashTimeout overrides the default 120s bash timeout (0 = default).
	// The per-call timeout argument still caps at 600s.
	BashTimeout time.Duration
	// BackgroundThreshold is how long a bash call may run before the
	// loop backgrounds it (0 = never background; DefaultBackgroundThreshold
	// when Setup leaves it unset).
	BackgroundThreshold time.Duration
	// Audit receives one entry per bash execution and denylist denial
	// (nil = no audit). Approval decisions are audited by the loop.
	Audit func(AuditEntry)
	// ImageInput is set from the selected model's config input: true
	// only when the model declares "image". LoadImage and image
	// reads refuse when it is false, so a text-only model never
	// receives image bytes it cannot use.
	ImageInput bool
	// Sandbox runs bash calls under bwrap (opt-in, --sandbox): the
	// workspace root is the only writable directory. BwrapPath names
	// the bwrap binary ("" = "bwrap" on PATH; Setup resolves it and
	// probes that a sandbox can actually be created).
	Sandbox   bool
	BwrapPath string
	// WebfetchTimeout bounds one webfetch call (0 = the 30s default).
	WebfetchTimeout time.Duration
	// HTTPClient is the client webfetch uses (nil = a default client
	// with the same timeout). Tests set it to reach a local server.
	HTTPClient *http.Client
	// LookupIP resolves hosts for webfetch's SSRF check (nil = the
	// system resolver). Tests inject a fake.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
	// OutputDir is where bash output files go (session-scoped, set
	// by app.Setup; "" = disabled, bash falls back to the in-memory
	// result). Read allows absolute paths under it even though they
	// sit outside the workspace root; Write/Edit/Glob/Grep do not.
	OutputDir string
	// Plugins runs plugin-registered tools (nil = no plugins loaded).
	// The interface keeps this package free of the moejs dependency:
	// app.Setup sets it to the loaded *plugins.Registry. A plugin
	// tool needs no approval: it runs JS only, inside its runtime,
	// with its file access jailed to its own data dir.
	Plugins PluginRunner

	jobsMu      sync.Mutex
	jobs        *JobRegistry
	sessionJobs map[string]*JobRegistry
	sessionID   string

	outMu  sync.Mutex
	outSeq int

	cacheOnce  sync.Once
	fetchCache *webfetchCache
}

// webfetchCache returns the executor's page cache, creating it on
// first use. The cache is per-executor (per run), never shared
// between tests or runs.
func (e *Executor) webfetchCache() *webfetchCache {
	e.cacheOnce.Do(func() { e.fetchCache = &webfetchCache{entries: map[string]*webfetchCacheEntry{}} })
	return e.fetchCache
}

func (e *Executor) audit(entry AuditEntry) {
	if e.Audit != nil {
		entry.TS = time.Now()
		e.Audit(entry)
	}
}

func (e *Executor) resolve(p string) (string, error) {
	return fileguard.Validate(e.Root, p)
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
	e.editMu.Lock()
	defer e.editMu.Unlock()
	base := e.Root
	if dir, ok := e.outputDirBase(a.Path); ok {
		base = dir
	}
	f, err := fileguard.Open(base, a.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		entries, err := f.ReadDir(-1)
		if err != nil {
			return "", err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
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
	if base == e.Root {
		// Hash through the open, jailed descriptor; the edit checks this exact version.
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return "", err
		}
		var digest [32]byte
		copy(digest[:], h.Sum(nil))
		if e.snapshots == nil {
			e.snapshots = map[string][32]byte{}
		}
		key, _ := filepath.Abs(a.Path)
		if !filepath.IsAbs(a.Path) {
			key = filepath.Join(e.Root, filepath.Clean(a.Path))
		}
		e.snapshots[key] = digest
	}
	start := max(a.Offset, 0)
	reader := bufio.NewReader(f)
	var out strings.Builder
	for line := 0; ; line++ {
		var data []byte
		eof := false
		clipped := false
		for {
			part, err := reader.ReadSlice('\n')
			if line >= start {
				take := min(len(part), MaxReadBytes-len(data))
				clipped = clipped || take < len(part)
				data = append(data, part[:take]...)
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil && err != io.EOF {
				return "", err
			}
			eof = err == io.EOF
			break
		}
		if line >= start {
			fmt.Fprintf(&out, "%d\t%s\n", line+1, strings.TrimSuffix(string(data), "\n"))
			if clipped {
				out.WriteString("... [line truncated at read byte limit]\n")
				return out.String(), nil
			}
			if (a.Limit > 0 && line-start+1 >= a.Limit) || eof {
				return out.String(), nil
			}
			if out.Len() >= MaxReadBytes {
				out.WriteString("... [truncated: use offset to continue]\n")
				return out.String(), nil
			}
		}
		if eof {
			return out.String(), nil
		}
	}
}

type WriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Overwrite replaces an existing file instead of failing.
	// Default false: writing to an existing file is an error, and
	// the model should use edit. Set it only to replace a whole
	// file; a small change belongs in edit.
	Overwrite bool `json:"overwrite,omitempty"`
}

func (e *Executor) Write(a WriteArgs) (string, error) {
	e.editMu.Lock()
	defer e.editMu.Unlock()
	defer e.invalidateSnapshot(a.Path)
	r, rel, err := fileguard.Root(e.Root, a.Path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	_, statErr := r.Stat(rel)
	if statErr == nil && !a.Overwrite {
		return "", fmt.Errorf("file exists, use edit instead: %s", a.Path)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	if err := r.MkdirAll(filepath.Dir(rel), 0755); err != nil {
		return "", err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if a.Overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := r.OpenFile(rel, flags, 0644)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(a.Content)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if statErr == nil {
		return "overwrote " + a.Path, nil
	}
	return "wrote " + a.Path, nil
}

type EditArgs struct {
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
	// StartLine/EndLine are the line-range form (1-based, inclusive):
	// when StartLine > 0 the edit replaces exactly those lines with
	// NewText after validating the last read version. EndLine 0 means StartLine (one
	// line). A line edit requires an unchanged file since its last read.
	StartLine int `json:"startLine,omitempty"`
	EndLine   int `json:"endLine,omitempty"`
}

// splitFileLines splits file content into lines without terminators.
// A trailing newline does not create an extra line: "a\nb\n" is 2
// lines, and the returned parts keep the trailing "" sentinel so a
// rejoin preserves it.
func splitFileLines(s string) ([]string, int) {
	if s == "" {
		return nil, 0
	}
	parts := strings.Split(s, "\n")
	n := len(parts)
	if strings.HasSuffix(s, "\n") {
		n--
	}
	return parts, n
}

func (e *Executor) Edit(a EditArgs) (string, error) {
	p, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	data, err := fileguard.ReadFile(e.Root, p)
	if err != nil {
		return "", err
	}
	s := string(data)
	if a.StartLine > 0 {
		return e.editLines(p, s, a)
	}
	if a.StartLine < 0 {
		return "", fmt.Errorf("startLine must be >= 1, got %d in %s", a.StartLine, a.Path)
	}
	if n := strings.Count(s, a.OldText); n != 1 {
		return "", editMatchError(a.Path, s, a.OldText, n)
	}
	if err := e.writeExisting(p, []byte(strings.Replace(s, a.OldText, a.NewText, 1))); err != nil {
		return "", err
	}
	return "edited " + a.Path, nil
}

// editLines replaces lines StartLine..EndLine (1-based, inclusive)
// with NewText. The file's trailing-newline behaviour is preserved:
// the split keeps the trailing "" sentinel, and NewText's own
// trailing newline is honoured the same way.
func (e *Executor) editLines(p, s string, a EditArgs) (string, error) {
	parts, n := splitFileLines(s)
	end := a.EndLine
	if end == 0 {
		end = a.StartLine
	}
	switch {
	case a.StartLine < 1:
		return "", fmt.Errorf("startLine must be >= 1, got %d in %s (file has %d lines)", a.StartLine, a.Path, n)
	case end < a.StartLine:
		return "", fmt.Errorf("endLine %d is before startLine %d in %s (file has %d lines)", end, a.StartLine, a.Path, n)
	case end > n:
		return "", fmt.Errorf("endLine %d is beyond the last line (%d) in %s", end, n, a.Path)
	}
	newParts := make([]string, 0, len(parts)+1)
	newParts = append(newParts, parts[:a.StartLine-1]...)
	newParts = append(newParts, strings.Split(a.NewText, "\n")...)
	newParts = append(newParts, parts[end:]...)
	if err := e.writeExisting(p, []byte(strings.Join(newParts, "\n"))); err != nil {
		return "", err
	}
	return "edited " + a.Path, nil
}

// editMatchError is the oldText failure: the existing greppable
// prefix, plus a diagnostic naming where to look, so the caller can
// fix the call instead of re-reading the whole file blind. The
// diagnostic is capped so a huge file cannot bloat the observation.
func editMatchError(path, content, oldText string, n int) error {
	base := fmt.Sprintf("oldText must match exactly once, got %d matches in %s", n, path)
	if strings.TrimSpace(oldText) == "" {
		return errors.New(base)
	}
	var diag string
	if n == 0 {
		diag = editNoMatchDiag(content, oldText)
	} else {
		diag = editMultiMatchDiag(content, oldText, n)
	}
	if diag == "" {
		return errors.New(base)
	}
	if r := []rune(diag); len(r) > 800 {
		diag = string(r[:800]) + "… [diagnostic truncated]"
	}
	return fmt.Errorf("%s; %s", base, diag)
}

// editNoMatchDiag locates a 0-match oldText: first, whether its first
// non-empty line occurs anywhere in the file (even as a substring);
// otherwise the closest-matching region by trimmed-line comparison.
func editNoMatchDiag(content, oldText string) string {
	var first string
	for _, l := range strings.Split(oldText, "\n") {
		if strings.TrimSpace(l) != "" {
			first = strings.TrimSpace(l)
			break
		}
	}
	if first != "" {
		var at []int
		for i, l := range strings.Split(content, "\n") {
			if strings.Contains(l, first) {
				at = append(at, i+1)
				if len(at) >= 10 {
					break
				}
			}
		}
		if len(at) > 0 {
			return fmt.Sprintf("first line %q occurs at line(s) %s — use those lines (startLine/endLine) or copy oldText from there", first, intsString(at))
		}
	}
	return editClosestRegion(content, oldText)
}

// editClosestRegion finds the longest run of oldText's lines (leading
// whitespace ignored on both sides) occurring contiguously in the
// file, and shows that region with line numbers.
func editClosestRegion(content, oldText string) string {
	trim := func(lines []string) []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = strings.TrimLeft(l, " \t")
		}
		return out
	}
	raw, n := splitFileLines(content)
	fileLines := trim(raw[:n])
	oldLines := trim(strings.Split(oldText, "\n"))
	// Longest contiguous run of oldText's lines (at any offset in
	// oldText) occurring in the file: the region the caller almost
	// wrote. A run must start at oldLines[j], not only at oldLines[0]
	// — a stale oldText usually matches from the second line on.
	bestStart, bestRun := -1, 0
	for i := 0; i < len(fileLines); i++ {
		for j := 0; j < len(oldLines); j++ {
			run := 0
			for k := 0; j+k < len(oldLines) && i+k < len(fileLines); k++ {
				if fileLines[i+k] != oldLines[j+k] {
					break
				}
				run++
			}
			if run > bestRun {
				bestRun, bestStart = run, i
			}
		}
	}
	if bestRun == 0 {
		return fmt.Sprintf("no line of oldText matches any line in the file (%d lines total)", n)
	}
	lo := bestStart - 1
	if lo < 0 {
		lo = 0
	}
	hi := bestStart + bestRun + 1
	if hi-lo > 10 {
		hi = lo + 10
	}
	if hi > n {
		hi = n
	}
	var b strings.Builder
	fmt.Fprintf(&b, "closest match (%d of %d lines) around line %d:\n", bestRun, len(oldLines), bestStart+1)
	for i := lo; i < hi; i++ {
		line := raw[i]
		if len(line) > 120 {
			line = line[:120] + "…"
		}
		fmt.Fprintf(&b, "%d: %s\n", i+1, line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// editMultiMatchDiag lists the lines where oldText matches, so the
// caller can disambiguate with a longer oldText or a line range.
func editMultiMatchDiag(content, oldText string, n int) string {
	var at []int
	for off := 0; ; {
		i := strings.Index(content[off:], oldText)
		if i < 0 {
			break
		}
		abs := off + i
		at = append(at, strings.Count(content[:abs], "\n")+1)
		off = abs + len(oldText)
		if len(at) >= 20 {
			break
		}
	}
	if len(at) == 0 {
		return ""
	}
	s := "matches at line(s) " + intsString(at)
	if n > len(at) {
		s += fmt.Sprintf(" (and %d more)", n-len(at))
	}
	return s
}

func intsString(ns []int) string {
	parts := make([]string, len(ns))
	for i, v := range ns {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

type BashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 120, max 600
	// Cwd runs the command in this directory instead of the
	// workspace root. A relative path resolves against the root.
	// Every bash call is a fresh shell starting in the root, and
	// cd does not persist between calls, so a bare `npm run build`
	// in a nested project fails with "Missing script" — pass cwd
	// (or prefix the command with `cd <dir> && `) instead.
	Cwd string `json:"cwd,omitempty"`
	// Background starts the command in the background and returns
	// at once with a job id, for commands that will not exit on
	// their own (dev server, watcher). The result is delivered
	// at the next step boundary, like any backgrounded job.
	// With no per-call timeout, an explicit background job has NO
	// timeout: a dev server must outlive any cap (a per-call
	// timeout, when given, is still honored).
	Background bool `json:"background,omitempty"`
}

// bashDir resolves the effective working directory for one bash
// call: the per-call cwd argument, or the workspace root when
// unset. A relative cwd resolves against the root. It must exist
// and be a directory; under the sandbox it must also stay inside
// the root, because only the root is bound into the jail. The
// shell itself is not jailed outside the sandbox — an approved
// bash call can cd anywhere — so cwd is validated, not jailed.
func (e *Executor) bashDir(a BashArgs) (string, error) {
	dir := e.Root
	if strings.TrimSpace(a.Cwd) != "" {
		dir = a.Cwd
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(e.Root, dir)
		}
		dir = filepath.Clean(dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("invalid cwd %q: %v", a.Cwd, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("invalid cwd %q: not a directory", a.Cwd)
	}
	if e.Sandbox {
		rel, err := filepath.Rel(e.Root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("invalid cwd %q: outside workspace root (sandbox)", a.Cwd)
		}
	}
	return dir, nil
}

// bashTimeout resolves the effective timeout for one bash call:
// per-call argument (capped at 600s), else the executor default.
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
	res, err := e.bashResult(ctx, a)
	return res.Text, err
}

func (e *Executor) bashResult(ctx context.Context, a BashArgs) (ToolResult, error) {
	if reason := DeniedCommand(a.Command); reason != "" {
		e.audit(AuditEntry{Kind: "denied", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: reason})
		return ToolResult{}, fmt.Errorf("denied by policy: %s", reason)
	}
	dir, err := e.bashDir(a)
	if err != nil {
		return ToolResult{}, err
	}
	path := e.nextOutputFile()
	out, code, err := e.runBashCmd(ctx, a.Command, e.bashTimeout(a), path, dir)
	entry := AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: code}
	if err != nil {
		entry.Error = err.Error()
	}
	e.audit(entry)
	return ToolResult{Text: out, ExitCode: &code, OutputFile: path}, err
}

// ToolResult is one tool call's observation. Images is set only by
// read on an image file: the text is the marker, and the loop
// attaches the image itself to the next request as a user message
// (tool messages cannot carry images in any of the three APIs).
// Detail is display-only content for the timeline (never sent to the
// model): write carries the written content, edit a -/+ diff of the
// replaced lines. Count is the number of matches (grep) or files
// (glob) found; only those two tools set it, and the TUI reads it
// only for them.
type ResultStatus string

const (
	ResultSuccess    ResultStatus = "success"
	ResultError      ResultStatus = "error"
	ResultBackground ResultStatus = "background"
)

type ToolResult struct {
	Status     ResultStatus
	ExitCode   *int
	OutputFile string
	Text       string
	Images     []llm.Image
	Detail     string
	Count      int
}

// maxDetailChars caps display-only detail: a huge write must not
// bloat the event stream (the model never sees it, but the TUI and
// the RPC client do).
const maxDetailChars = 4096

// capDetail truncates display detail with a marker.
func capDetail(s string) string {
	if len([]rune(s)) <= maxDetailChars {
		return s
	}
	return string([]rune(s)[:maxDetailChars]) + "\n[… truncated: detail continues, first 4 KiB shown]"
}

// diffDetail renders an edit as a small -/+ diff: the removed lines,
// then the added lines. No context lines — the replaced block is
// the whole change.
func diffDetail(oldText, newText string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(oldText, "\n"), "\n") {
		b.WriteString("− " + l + "\n")
	}
	for _, l := range strings.Split(strings.TrimRight(newText, "\n"), "\n") {
		b.WriteString("+ " + l + "\n")
	}
	return capDetail(strings.TrimRight(b.String(), "\n"))
}

// PluginRunner runs plugin-registered tools. The plugins package
// implements it; the tools package only knows this interface.
type PluginRunner interface {
	Has(name string) bool
	Call(ctx context.Context, name, argsJSON string) (string, error)
	// RunToolCallHooks runs the registered tool_call hooks in load
	// order, before approval. It returns the (possibly rewritten)
	// args JSON and a denial: "" when the call is allowed, otherwise
	// "plugin <name>: <reason>". A hook that throws or times out
	// denies too (fail closed), with the failure as the reason.
	// The first denial stops the chain; arg rewrites chain on.
	RunToolCallHooks(ctx context.Context, tool, argsJSON string) (string, string)
	// RunToolResultHooks runs the registered tool_result hooks in
	// load order, each seeing the previous one's rewrite, and returns
	// the observation to use. A hook that throws, times out, or
	// returns a non-string keeps the current result (fail open);
	// the failure is logged to stderr by the implementation.
	RunToolResultHooks(ctx context.Context, tool, argsJSON, result string, toolErr error) string
	// RunStopHooks runs the registered stop hooks in load order
	// when a turn would otherwise end. It returns the first block
	// reason ("" = the turn may end): a hook returns a non-empty
	// string to veto the turn end — typically a check the final
	// answer failed, e.g. "go test failed". A hook that throws,
	// times out, or returns a non-string is skipped with a stderr
	// warning (fail open: a broken check must not endlessly block
	// a turn).
	RunStopHooks(ctx context.Context, session, lastText string) string
}

// Dispatch runs a tool call by name and returns the observation.
// A read of an image file (png, jpg, gif, webp) returns the image
// alongside its marker text instead of the file's bytes as text.
func (e *Executor) Dispatch(ctx context.Context, name, argsJSON string) (result ToolResult, resultErr error) {
	defer func() {
		if resultErr != nil {
			result.Status = ResultError
		} else if result.Status == "" {
			result.Status = ResultSuccess
		}
	}()
	switch name {
	case "read":
		var a ReadArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad read args: %w", err)
		}
		if _, ok := MediaTypeForPath(a.Path); ok {
			// The model chose the path, so the workspace jail
			// still applies here (unlike -image, which the user
			// named themselves). Resolve first, then load.
			if !e.ImageInput {
				return ToolResult{}, fmt.Errorf("image input is not enabled for this model: add \"image\" to the model's input in the config file")
			}
			img, err := e.ReadImage(a.Path)
			if err != nil {
				return ToolResult{}, err
			}
			return ToolResult{
				Text:   fmt.Sprintf("Image %s — attached as an image below.", img.Describe()),
				Images: []llm.Image{img},
			}, nil
		}
		out, err := e.Read(a)
		return ToolResult{Text: out}, err
	case "write":
		var a WriteArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad write args: %w", err)
		}
		out, err := e.Write(a)
		if err != nil {
			return ToolResult{Text: out}, err
		}
		return ToolResult{Text: out, Detail: capDetail(a.Content)}, nil
	case "edit":
		var a EditArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad edit args: %w", err)
		}
		// For a line edit the diff needs the removed lines, which
		// Edit overwrites: read them first (cheap, local, and the
		// bounds are re-checked by Edit itself).
		removed := a.OldText
		if a.StartLine > 0 {
			if p, rerr := e.resolve(a.Path); rerr == nil {
				if data, rerr := fileguard.ReadFile(e.Root, p); rerr == nil {
					lines, n := splitFileLines(string(data))
					end := a.EndLine
					if end == 0 {
						end = a.StartLine
					}
					if a.StartLine >= 1 && end >= a.StartLine && end <= n {
						removed = strings.Join(lines[a.StartLine-1:end], "\n")
					}
				}
			}
		}
		out, err := e.Edit(a)
		if err != nil {
			return ToolResult{Text: out}, err
		}
		return ToolResult{Text: out, Detail: diffDetail(removed, a.NewText)}, nil
	case "bash":
		var a BashArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad bash args: %w", err)
		}
		return e.bashResult(ctx, a)
	case "grep":
		var a GrepArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad grep args: %w", err)
		}
		out, n, err := e.Grep(ctx, a)
		return ToolResult{Text: out, Count: n}, err
	case "glob":
		var a GlobArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad glob args: %w", err)
		}
		out, n, err := e.Glob(ctx, a)
		return ToolResult{Text: out, Count: n}, err
	case "load_skill":
		var a LoadSkillArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad load_skill args: %w", err)
		}
		out, err := e.LoadSkill(a)
		return ToolResult{Text: out}, err
	case "webfetch":
		var a WebfetchArgs
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return ToolResult{}, fmt.Errorf("bad webfetch args: %w", err)
		}
		out, err := e.Webfetch(ctx, a)
		return ToolResult{Text: out}, err
	default:
		if e.Plugins != nil && e.Plugins.Has(name) {
			out, err := e.Plugins.Call(ctx, name, argsJSON)
			return ToolResult{Text: out}, err
		}
		return ToolResult{}, fmt.Errorf("unknown tool: %s", name)
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

func (e *Executor) writeExisting(path string, data []byte) error {
	r, rel, err := fileguard.Root(e.Root, path)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.WriteFile(rel, data, 0644)
}

func (e *Executor) invalidateSnapshot(path string) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.Root, filepath.Clean(path))
	}
	delete(e.snapshots, path)
}
