// Background bash jobs (P2-8). A bash call that runs longer than the
// background threshold keeps running in a goroutine instead of blocking
// the agent loop; the loop feeds its result back when it finishes.
// The denylist, timeout, output cap and audit log are the same as a
// synchronous Bash call — backgrounding changes when the result is
// delivered, never what is allowed to run.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// timeoutError marks a bash run killed by its timeout. Its message is
// the same "bash timed out after …" the synchronous path returns, so
// callers can tell it apart from an exit failure without parsing text.
type timeoutError struct{ timeout time.Duration }

func (e *timeoutError) Error() string { return fmt.Sprintf("bash timed out after %s", e.timeout) }

// JobStatus is running until the command exits (or times out).
type JobStatus string

const (
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
)

// JobResult is what a finished background bash produced.
type JobResult struct {
	Output   string
	Err      error
	ExitCode int
}

// Job is one background bash execution.
type Job struct {
	ID      string
	Command string
	// OutputFile is the per-call file holding the full stdout+stderr
	// ("" when OutputDir was unset); it grows while the job runs.
	OutputFile string
	Started    time.Time

	mu         sync.Mutex
	done       chan struct{}
	result     JobResult
	finished   bool
	finishedAt time.Time
}

// Done closes when the command finishes (success, failure or timeout).
func (j *Job) Done() <-chan struct{} { return j.done }

// FinishedAt reports when the job finished; ok is false while running.
func (j *Job) FinishedAt() (time.Time, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finishedAt, j.finished
}

// Status reports running or done.
func (j *Job) Status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished {
		return JobDone
	}
	return JobRunning
}

// Result returns the outcome once finished; ok is false while running.
func (j *Job) Result() (JobResult, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result, j.finished
}

// Elapsed is how long the job has been running, or how long it ran
// for once finished (it stops growing at completion).
func (j *Job) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished {
		return j.finishedAt.Sub(j.Started)
	}
	return time.Since(j.Started)
}

func (j *Job) finish(r JobResult) {
	j.mu.Lock()
	j.result = r
	j.finished = true
	j.finishedAt = time.Now()
	j.mu.Unlock()
	close(j.done)
}

// JobRegistry tracks every background job started by one Executor
// (one process). Completed jobs stay listed so /jobs can show them.
type JobRegistry struct {
	mu   sync.Mutex
	jobs []*Job
	next int
}

func (r *JobRegistry) add(j *Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	j.ID = fmt.Sprintf("bg-%d", r.next)
	r.jobs = append(r.jobs, j)
	// Bound memory: drop the oldest finished jobs past 50.
	if len(r.jobs) > 50 {
		kept := r.jobs[:0]
		dropped := 0
		for _, old := range r.jobs {
			if dropped < len(r.jobs)-50 && old.Status() == JobDone {
				dropped++
				continue
			}
			kept = append(kept, old)
		}
		r.jobs = kept
	}
}

// List returns a snapshot of all jobs, oldest first.
func (r *JobRegistry) List() []*Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Job, len(r.jobs))
	copy(out, r.jobs)
	return out
}

// Get returns the job with the given id, or nil.
func (r *JobRegistry) Get(id string) *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// forget removes a job from the list. The loop uses it for a bash call
// that finished within the threshold: it never backgrounded, so it
// does not belong in /jobs.
func (r *JobRegistry) forget(j *Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, old := range r.jobs {
		if old == j {
			r.jobs = append(r.jobs[:i], r.jobs[i+1:]...)
			return
		}
	}
}

// ForgetJob removes a job from this Executor's list (see forget).
func (e *Executor) ForgetJob(j *Job) { e.registry().forget(j) }

// runBashCmd runs one bash -c command with the given timeout and
// returns the inline (head+tail) result, exit code and error. A
// timeout is reported as a *timeoutError; a non-zero exit as an
// error wrapped with "exit: ". Shared by Bash and BashBackground so
// both audit the same outcomes. With Sandbox on, the command runs
// under bwrap (see sandbox.go); killing the bwrap process kills the
// sandbox too (--die-with-parent).
//
// When outPath is set, combined stdout+stderr streams to that file
// as it is produced (so a background job's file grows while it
// runs) and the returned string is FormatBashOutput's head+tail
// view plus the path. When outPath is "" (no OutputDir), the old
// in-memory behaviour applies: CombinedOutput capped at
// MaxOutBytes, no path.
func (e *Executor) runBashCmd(ctx context.Context, command string, timeout time.Duration, outPath string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if e.Sandbox {
		bwrap := e.BwrapPath
		if bwrap == "" {
			bwrap = "bwrap"
		}
		args := append(sandboxArgs(e.Root), "--", "bash", "-c", command)
		cmd = exec.CommandContext(ctx, bwrap, args...)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = e.Root
	}
	if outPath != "" {
		sink, err := newBashOutputSink(outPath)
		if err == nil {
			cmd.Stdout = sink
			cmd.Stderr = sink
			runErr := cmd.Run()
			_ = sink.Close()
			lines, bytes, fileCapped, head, tail := sink.snapshot()
			s := FormatBashOutput(lines, bytes, fileCapped, head, tail, outPath)
			exitCode := -1
			if cmd.ProcessState != nil {
				exitCode = cmd.ProcessState.ExitCode()
			}
			if ctx.Err() == context.DeadlineExceeded {
				return s, -1, &timeoutError{timeout}
			}
			if runErr != nil {
				return s, exitCode, fmt.Errorf("exit: %w", runErr)
			}
			return s, exitCode, nil
		}
		// Unwritable output file must not fail the command: fall
		// through to the in-memory path in this same invocation
		// (never re-run the command).
	}
	out, err := cmd.CombinedOutput()
	s := truncate(string(out), MaxOutBytes)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return s, -1, &timeoutError{timeout}
	}
	if err != nil {
		return s, exitCode, fmt.Errorf("exit: %w", err)
	}
	return s, exitCode, nil
}

// BashBackground starts a bash command in a goroutine and returns its
// Job immediately. The denylist is checked before anything runs (a
// denied command returns an error, never a job); the timeout, output
// cap and audit entries are the same as a synchronous Bash call, and
// the audit entry is written when the command finishes, with its real
// exit code.
func (e *Executor) BashBackground(ctx context.Context, a BashArgs) (*Job, error) {
	if reason := DeniedCommand(a.Command); reason != "" {
		e.audit(AuditEntry{Kind: "denied", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: reason})
		return nil, fmt.Errorf("denied by policy: %s (this is harm reduction, not a sandbox — see README)", reason)
	}
	timeout := e.bashTimeout(a)
	outPath := e.nextOutputFile()
	job := &Job{Command: a.Command, OutputFile: outPath, Started: time.Now(), done: make(chan struct{})}
	e.registry().add(job)
	go func() {
		out, exitCode, err := e.runBashCmd(ctx, a.Command, timeout, outPath)
		var te *timeoutError
		switch {
		case errors.As(err, &te):
			e.audit(AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: fmt.Sprintf("timed out after %s", timeout)})
		case err != nil:
			e.audit(AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: exitCode, Error: err.Error()})
		default:
			e.audit(AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: exitCode})
		}
		job.finish(JobResult{Output: out, Err: err, ExitCode: exitCode})
	}()
	return job, nil
}

// Jobs returns a snapshot of all background jobs for this Executor.
func (e *Executor) Jobs() []*Job { return e.registry().List() }

func (e *Executor) registry() *JobRegistry {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	if e.jobs == nil {
		e.jobs = &JobRegistry{}
	}
	return e.jobs
}
