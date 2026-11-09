package tools

import (
	"context"
	"strings"
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
