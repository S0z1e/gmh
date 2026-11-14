package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A slow command started in the background returns immediately; its
// result arrives on Done, and the completion is audited with the real
// exit code.
func TestBashBackgroundReturnsEarlyAndDelivers(t *testing.T) {
	root := t.TempDir()
	var entries []AuditEntry
	e := &Executor{Root: root, Audit: func(en AuditEntry) { entries = append(entries, en) }}

	start := time.Now()
	job, err := e.BashBackground(context.Background(), BashArgs{Command: "sleep 1; echo BG-OK"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("BashBackground took %v, want it to return at once", time.Since(start))
	}
	if job.ID != "bg-1" {
		t.Fatalf("job id = %q, want bg-1", job.ID)
	}
	if job.Status() != JobRunning {
		t.Fatalf("job status = %s, want running", job.Status())
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	res, ok := job.Result()
	if !ok || res.Err != nil || !strings.Contains(res.Output, "BG-OK") {
		t.Fatalf("result = %+v, ok = %v", res, ok)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if job.Status() != JobDone {
		t.Fatalf("job status = %s, want done", job.Status())
	}
	if len(entries) != 1 || entries[0].Kind != "bash" || entries[0].ExitCode != 0 {
		t.Fatalf("audit entries = %+v, want one bash entry with exit 0", entries)
	}
	if got := e.Jobs(); len(got) != 1 || got[0] != job {
		t.Fatalf("Jobs() = %v, want the one job", got)
	}
}

// A denylisted command is refused at once and never becomes a job.
func TestBashBackgroundDenylistRefuses(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	if _, err := e.BashBackground(context.Background(), BashArgs{Command: "rm -rf /"}); err == nil {
		t.Fatal("denylisted command should error")
	}
	if got := e.Jobs(); len(got) != 0 {
		t.Fatalf("Jobs() = %v, want none", got)
	}
}

// The per-call timeout still kills a background job, and the timeout
// is audited like a synchronous one.
func TestBashBackgroundTimeout(t *testing.T) {
	var entries []AuditEntry
	e := &Executor{Root: t.TempDir(), Audit: func(en AuditEntry) { entries = append(entries, en) }}
	job, err := e.BashBackground(context.Background(), BashArgs{Command: "sleep 5", Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timed-out job did not finish")
	}
	res, _ := job.Result()
	if res.Err == nil {
		t.Fatal("timed-out job should carry an error")
	}
	if !strings.Contains(res.Err.Error(), "timed out after 1s") {
		t.Fatalf("error = %v, want a timeout", res.Err)
	}
	if len(entries) != 1 || entries[0].ExitCode != -1 {
		t.Fatalf("audit entries = %+v, want one timeout entry", entries)
	}
}

// Job ids increase across the process and List keeps them in order.
func TestJobRegistryIds(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	for i := 0; i < 3; i++ {
		if _, err := e.BashBackground(context.Background(), BashArgs{Command: "echo x"}); err != nil {
			t.Fatal(err)
		}
	}
	jobs := e.Jobs()
	if len(jobs) != 3 || jobs[0].ID != "bg-1" || jobs[2].ID != "bg-3" {
		t.Fatalf("jobs = %v", jobs)
	}
	// Wait for all three so no goroutine outlives the test.
	for _, j := range jobs {
		<-j.Done()
	}
}

// A command whose bash child exits at once but leaves a grandchild
// holding stdout open must not block the call: on the old code
// cmd.Wait() waited for the pipe to close, so the call hung until
// the grandchild exited (the café deadlock: a dev server started
// with `&` blocked turn end forever).
func TestBashGrandchildDoesNotBlockWait(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = e.Bash(context.Background(), BashArgs{Command: `(sleep 30 &) ; echo done`, Timeout: 60})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("bash call blocked by a grandchild holding stdout (the pipe trap)")
	}
	if err != nil {
		t.Fatalf("bash error = %v", err)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("output = %q, want it to contain done", out)
	}
}

// A background job must survive its caller's context being
// cancelled: the turn ctx is cancelled after EVERY turn (even a
// normally settled one — runTurn calls cancel() after Handle), so
// tying the job to it killed a dev server the moment its turn
// settled. Proven live 2026-11-13: the café preview server
// answered curl 200 during the turn and was gone right after it
// settled, because the turn-end cancel fired cmd.Cancel, and the
// group kill then killed the whole tree.
func TestBashBackgroundSurvivesContextCancel(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	job, err := e.BashBackground(ctx, BashArgs{Command: "sleep 2; echo STILL-ALIVE"})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // like runTurn's cancel() after a settled turn
	time.Sleep(500 * time.Millisecond)
	if _, ok := job.Result(); ok {
		t.Fatal("background job finished after its context was cancelled; it must outlive the turn")
	}
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish on its own")
	}
	res, _ := job.Result()
	if res.Err != nil || !strings.Contains(res.Output, "STILL-ALIVE") {
		t.Fatalf("result = %+v, want a clean finish with STILL-ALIVE", res)
	}
}

// A timeout must kill the whole process tree, not just the direct
// bash child: a grandchild that outlives the timeout must be dead
// when the call returns.
func TestBashTimeoutKillsGrandchild(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "grandchild.pid")
	e := &Executor{Root: root, OutputDir: t.TempDir()}
	start := time.Now()
	_, err := e.Bash(context.Background(), BashArgs{
		Command: `sleep 30 & echo $! > ` + pidFile + `; wait`,
		Timeout: 2,
	})
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("bash call took %v, want it to return near the 2s timeout", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a timeout", err)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	// Process death lags the kill by a scheduling quantum: Wait
	// returns when bash dies, but the grandchild's task may not
	// have finished dying yet (seen under load in the full run).
	// Poll briefly; a zombie counts as dead — it is dead, just
	// unreaped by init.
	deadline := time.Now().Add(5 * time.Second)
	for !procDead(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid %d still alive after the timeout kill", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// procDead reports whether the process is gone, or a zombie (dead,
// unreaped — Kill(pid, 0) still succeeds on a zombie).
func procDead(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // gone between the two checks
	}
	// State is the field after the closing paren of the comm
	// (the comm itself may contain ')' and spaces).
	if i := strings.LastIndexByte(string(data), ')'); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

// The cwd argument runs the command in that directory (relative
// paths resolve against the root), the result header names the
// directory it ran in, and a bad cwd is a clean tool error, never
// a crash. The default (no cwd) keeps running in the root.
func TestBashCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(root, "out")
	e := &Executor{Root: root, OutputDir: outDir}

	out, err := e.Bash(context.Background(), BashArgs{Command: "pwd", Cwd: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, sub+"\n") && !strings.Contains(out, sub) {
		t.Fatalf("pwd with cwd=proj:\n%s\nwant it to run in %s", out, sub)
	}
	if !strings.Contains(out, "cwd: "+sub) {
		t.Fatalf("result header has no cwd:\n%s", out)
	}

	out, err = e.Bash(context.Background(), BashArgs{Command: "pwd", Cwd: sub})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cwd: "+sub) {
		t.Fatalf("absolute cwd missing from header:\n%s", out)
	}

	// Default: the root, and the header says so.
	out, err = e.Bash(context.Background(), BashArgs{Command: "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cwd: "+root) {
		t.Fatalf("default cwd missing from header:\n%s", out)
	}

	// A nonexistent cwd and a cwd that is a file are tool errors.
	if _, err := e.Bash(context.Background(), BashArgs{Command: "pwd", Cwd: "nope"}); err == nil || !strings.Contains(err.Error(), "invalid cwd") {
		t.Fatalf("missing cwd error = %v, want an 'invalid cwd' error", err)
	}
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bash(context.Background(), BashArgs{Command: "pwd", Cwd: "a.txt"}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file cwd error = %v, want a 'not a directory' error", err)
	}
	// A background job with a bad cwd fails at start, never in a goroutine.
	if _, err := e.BashBackground(context.Background(), BashArgs{Command: "pwd", Cwd: "nope"}); err == nil || !strings.Contains(err.Error(), "invalid cwd") {
		t.Fatalf("background bad cwd error = %v, want an 'invalid cwd' error", err)
	}
}

func TestCapturedCancelOnlyStopsOwnerSession(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	e.SwitchSession("first")
	first, err := e.BashBackground(context.Background(), BashArgs{Command: "sleep 10"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cancel()
	cancelFirst := e.SessionCanceller()
	e.SwitchSession("second")
	second, err := e.BashBackground(context.Background(), BashArgs{Command: "sleep 10"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cancel()
	cancelFirst()
	select {
	case <-first.Done():
	case <-time.After(time.Second):
		t.Fatal("owner job survived cancel")
	}
	select {
	case <-second.Done():
		t.Fatal("old cancel stopped the new session job")
	default:
	}
}
