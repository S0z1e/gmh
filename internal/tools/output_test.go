package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Short output is returned whole, with the header naming the file.
func TestFormatBashOutputShort(t *testing.T) {
	head := []string{"line 1", "line 2"}
	got := FormatBashOutput(2, 13, false, head, head, "/sessions/s1/bash-output/bash-1-1.log")
	if !strings.Contains(got, "line 1\nline 2") {
		t.Fatalf("short output not whole:\n%s", got)
	}
	if !strings.Contains(got, "[bash output: 2 lines, 13 bytes; full output in /sessions/s1/bash-output/bash-1-1.log]") {
		t.Fatalf("header missing or wrong:\n%s", got)
	}
	if strings.Contains(got, "showing first") {
		t.Fatalf("short output should not be truncated:\n%s", got)
	}
}

// Long output shows only head + tail, with the totals and the path.
func TestFormatBashOutputLong(t *testing.T) {
	var head, tail []string
	for i := 1; i <= 30; i++ {
		head = append(head, fmt.Sprintf("head %d", i))
	}
	for i := 371; i <= 400; i++ {
		tail = append(tail, fmt.Sprintf("tail %d", i))
	}
	got := FormatBashOutput(400, 4000, false, head, tail, "/sessions/s1/bash-output/bash-1-2.log")
	if !strings.Contains(got, "head 1") || !strings.Contains(got, "head 30") {
		t.Fatalf("head missing:\n%s", got)
	}
	if !strings.Contains(got, "tail 371") || !strings.Contains(got, "tail 400") {
		t.Fatalf("tail missing:\n%s", got)
	}
	if strings.Contains(got, "head 31") || strings.Contains(got, "tail 370") {
		t.Fatalf("middle lines leaked into the inline result:\n%s", got)
	}
	if !strings.Contains(got, "showing first 30 and last 30 of 400 lines") {
		t.Fatalf("marker missing:\n%s", got)
	}
	if !strings.Contains(got, "400 lines, 4000 bytes; full output in /sessions/s1/bash-output/bash-1-2.log") {
		t.Fatalf("header missing:\n%s", got)
	}
}

// Empty output is just the header.
func TestFormatBashOutputEmpty(t *testing.T) {
	got := FormatBashOutput(0, 0, false, nil, nil, "/sessions/s1/bash-output/bash-1-3.log")
	if !strings.Contains(got, "[bash output: 0 lines, 0 bytes; full output in /sessions/s1/bash-output/bash-1-3.log]") {
		t.Fatalf("empty header wrong:\n%s", got)
	}
	if strings.Contains(got, "showing first") {
		t.Fatalf("empty output should not be truncated:\n%s", got)
	}
}

// One enormous line is capped inline (the file keeps it whole).
func TestFormatBashOutputHugeLine(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	got := FormatBashOutput(1, 100000, false, []string{capLine(huge)}, []string{capLine(huge)}, "/p.log")
	if len(got) > bashOutputInlineMaxChars+1000 {
		t.Fatalf("inline result = %d chars, want it bounded near %d", len(got), bashOutputInlineMaxChars)
	}
	if !strings.Contains(got, "line truncated") {
		t.Fatalf("huge line should be marked truncated:\n%s", got[:500])
	}
}

// The sink splits lines correctly across Write boundaries and keeps
// head + tail while a stream is still open. It no longer writes a
// file itself (the child writes straight to the file); the file
// round-trip is covered by TestScanOutputFile below.
func TestBashOutputSinkSplitWrites(t *testing.T) {
	sink := newBashOutputSink()
	// Deliberately split mid-line and mid-word.
	for _, chunk := range []string{"li", "ne 1\nline", " 2\nlin", "e 3\npartial"} {
		if _, err := sink.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	lines, _, _, head, tail := sink.snapshot()
	if lines != 3 {
		t.Fatalf("lines = %d, want 3 (partial line not counted until Close)", lines)
	}
	if len(head) != 3 || head[0] != "line 1" || head[2] != "line 3" {
		t.Fatalf("head = %v", head)
	}
	if len(tail) != 3 || tail[2] != "line 3" {
		t.Fatalf("tail = %v", tail)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	lines, _, _, _, _ = sink.snapshot()
	if lines != 4 {
		t.Fatalf("lines after Close = %d, want 4 (trailing partial line)", lines)
	}
}

// scanOutputFile re-derives the accounting from a file the child
// wrote directly: totals, head and tail, including a trailing
// partial line; a missing file is an error.
func TestScanOutputFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	var want strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&want, "line %d\n", i)
	}
	want.WriteString("partial")
	if err := os.WriteFile(path, []byte(want.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, bytes, capped, head, tail, err := scanOutputFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines != 101 {
		t.Fatalf("lines = %d, want 101", lines)
	}
	if bytes != int64(len(want.String())) {
		t.Fatalf("bytes = %d, want %d", bytes, len(want.String()))
	}
	if capped {
		t.Fatal("capped should be false for a small file")
	}
	if len(head) != 30 || head[0] != "line 1" || head[29] != "line 30" {
		t.Fatalf("head = %v", head)
	}
	if len(tail) != 30 || tail[0] != "line 72" || tail[29] != "partial" {
		t.Fatalf("tail = %v", tail)
	}
	if _, _, _, _, _, err := scanOutputFile(filepath.Join(t.TempDir(), "missing.log")); err == nil {
		t.Fatal("scanning a missing file should fail")
	}
}

// Concurrent stdout/stderr writers must not corrupt the line
// accounting.
func TestBashOutputSinkConcurrent(t *testing.T) {
	sink := newBashOutputSink()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				fmt.Fprintf(sink, "writer %d line %d\n", w, i)
			}
		}(w)
	}
	wg.Wait()
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	lines, _, _, _, _ := sink.snapshot()
	if lines != 100 {
		t.Fatalf("lines = %d, want 100", lines)
	}
}

// A background job's partial output is readable from its file —
// both directly and through the read tool — while the job is still
// running. The finished result is head+tail with the path, and the
// file holds the whole output.
func TestBashBackgroundPartialOutputReadable(t *testing.T) {
	root := t.TempDir()
	outDir := t.TempDir()
	e := &Executor{Root: root, OutputDir: outDir}
	job, err := e.BashBackground(context.Background(), BashArgs{
		Command: `for i in 1 2 3 4 5 6; do echo "line $i"; sleep 0.2; done`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.OutputFile == "" {
		t.Fatal("job has no output file")
	}
	if !strings.HasPrefix(job.OutputFile, outDir) {
		t.Fatalf("output file = %q, want it under %q", job.OutputFile, outDir)
	}
	// Poll until at least 2 lines are visible in the file while the
	// job is still running.
	deadline := time.Now().Add(5 * time.Second)
	seen := 0
	for time.Now().Before(deadline) {
		if job.Status() == JobDone {
			break
		}
		data, err := os.ReadFile(job.OutputFile)
		if err != nil {
			if os.IsNotExist(err) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			t.Fatal(err)
		}
		seen = len(strings.Split(strings.TrimRight(string(data), "\n"), "\n"))
		if seen >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if seen < 2 {
		t.Fatalf("only %d lines visible mid-run, want >= 2", seen)
	}
	if job.Status() != JobRunning {
		t.Fatalf("job already done after %d lines; the run must still be in progress", seen)
	}
	// The same partial output is reachable through the read tool,
	// whose jail would otherwise refuse a path outside the root.
	text, err := e.Read(ReadArgs{Path: job.OutputFile})
	if err != nil {
		t.Fatalf("read of output file failed: %v", err)
	}
	if !strings.Contains(text, "line 1") || !strings.Contains(text, "line 2") {
		t.Fatalf("read returned %q, want the partial lines", text)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	res, _ := job.Result()
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(res.Output, "[bash output: 6 lines") {
		t.Fatalf("result header missing:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, job.OutputFile) {
		t.Fatalf("result has no output path:\n%s", res.Output)
	}
	data, err := os.ReadFile(job.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		if !strings.Contains(string(data), fmt.Sprintf("line %d", i)) {
			t.Fatalf("file missing line %d:\n%s", i, string(data))
		}
	}
}

// The read exception is narrow: a file under OutputDir reads fine,
// a file outside both the root and OutputDir still fails, and
// Write into the session directory stays refused.
func TestReadOutputDirExceptionIsNarrow(t *testing.T) {
	root := t.TempDir()
	outDir := t.TempDir()
	e := &Executor{Root: root, OutputDir: outDir}
	if err := os.WriteFile(filepath.Join(outDir, "x.log"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := e.Read(ReadArgs{Path: filepath.Join(outDir, "x.log")}); err != nil || !strings.Contains(got, "hello") {
		t.Fatalf("read under OutputDir = %q, err = %v", got, err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Read(ReadArgs{Path: outside}); err == nil {
		t.Fatal("read outside root and OutputDir should fail")
	}
	if _, err := e.Write(WriteArgs{Path: filepath.Join(outDir, "evil.txt"), Content: "x"}); err == nil {
		t.Fatal("write into OutputDir should stay refused by the jail")
	}
	if _, err := e.Read(ReadArgs{Path: "x.log"}); err == nil {
		t.Fatal("relative path must not resolve into OutputDir")
	}
}

// Foreground bash with OutputDir writes the file and returns
// head+tail with the path; without OutputDir the old behaviour
// (whole output, no path) is unchanged.
func TestBashForegroundOutputFile(t *testing.T) {
	root := t.TempDir()
	outDir := t.TempDir()
	e := &Executor{Root: root, OutputDir: outDir}
	out, err := e.Bash(context.Background(), BashArgs{Command: "seq 1 200"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[bash output: 200 lines") || !strings.Contains(out, "full output in") {
		t.Fatalf("foreground result missing header:\n%s", out[:500])
	}
	if !strings.Contains(out, "200") || strings.Contains(out, "\n100\n") {
		t.Fatalf("foreground result should be head+tail, not the middle:\n%s", out)
	}
	files, err := os.ReadDir(outDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("output files = %v, err = %v", files, err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "200\n") || !strings.Contains(string(data), "1\n") {
		t.Fatalf("file missing full output:\n%s", string(data)[:200])
	}
}

// Without OutputDir, bash behaves exactly as before.
func TestBashNoOutputDirLegacy(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	out, err := e.Bash(context.Background(), BashArgs{Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi\n" {
		t.Fatalf("legacy output = %q, want \"hi\\n\"", out)
	}
	if strings.Contains(out, "full output in") {
		t.Fatalf("legacy output should carry no path:\n%s", out)
	}
}
