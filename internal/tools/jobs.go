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
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
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
	// delivered is set once the result was fed back to the model
	// (collectFinishedJobs or drainPending): a job re-attached by a
	// later turn must not deliver its result twice.
	delivered bool
	// reported is set once a bounded drain told the model the job is
	// still running: the next turn's drain must not wait on it again
	// (the turn would pay the bound on every turn while a server runs).
	reported bool
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

// Delivered reports whether the job's result was already fed back
// to the model.
func (j *Job) Delivered() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.delivered
}

// MarkDelivered records that the result was fed back to the model.
func (j *Job) MarkDelivered() {
	j.mu.Lock()
	j.delivered = true
	j.mu.Unlock()
}

// Reported reports whether a bounded drain already told the model
// this job is still running.
func (j *Job) Reported() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.reported
}

// MarkReported records that the still-running notice was fed back.
func (j *Job) MarkReported() {
	j.mu.Lock()
	j.reported = true
	j.mu.Unlock()
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

// IsTimeout reports whether err is a *timeoutError: a bash call
// killed by its timeout (the whole process group, grandchildren
// included).
func IsTimeout(err error) bool {
	var te *timeoutError
	return errors.As(err, &te)
}

// passed OutputFileMaxBytes. The old sink stopped recording at the
// cap but let the command run to its timeout; with the child
// writing straight to the file there is nothing to stop the writes
// except killing the runaway, so the cap is now a kill: the file
// keeps the first ~64 MiB and the call fails instead of filling
// the disk.
type outputCapError struct{ bytes int64 }

func (e *outputCapError) Error() string {
	return fmt.Sprintf("bash output exceeded %d bytes", OutputFileMaxBytes)
}

// runBashCmd runs one bash -c command with the given timeout and
// returns the inline (head+tail) result, exit code and error. A
// timeout is reported as a *timeoutError; a non-zero exit as an
// error wrapped with "exit: ". Shared by Bash and BashBackground so
// both audit the same outcomes. With Sandbox on, the command runs
// under bwrap (see sandbox.go); killing the bwrap process kills the
// sandbox too (--die-with-parent).
//
// The child's stdout+stderr go STRAIGHT to the output file (an
// *os.File, no pipe): giving exec a custom Writer would make it
// build an os.Pipe, and cmd.Wait() would block until every
// grandchild holding that pipe closed it — a dev server started
// with `&` blocked turn end forever (the café deadlock). The
// accounting (lines, bytes, head/tail) is re-derived from the file
// after Wait returns (scanOutputFile); a grandchild may still be
// appending then, which is fine — the scan is a point-in-time view
// and the file keeps growing for later reads.
//
// The child runs in its own process group (Setpgid), and the Cancel
// hook kills the whole group: a timeout (or an abort) terminates
// grandchildren too, instead of leaving a vite server orphaned and
// holding the output open. The group kill applies to the bwrap path
// as well — the sandbox shares the host pid namespace, so the group
// is reachable from here.
//
// A timeout <= 0 means no deadline (explicit background jobs
// without a per-call timeout): a dev server must outlive any cap.
//
// The output file is watched while the command runs: past
// OutputFileMaxBytes the process group is killed and the call fails
// with an *outputCapError, so a spewing command cannot fill the
// disk. When outPath is "" (no OutputDir), the same file mechanism
// runs against a temporary file that is deleted afterwards, and the
// result is the old in-memory behaviour: whole output capped at
// MaxOutBytes, no path.
func (e *Executor) runBashCmd(ctx context.Context, command string, timeout time.Duration, outPath, dir string) (string, int, error) {
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var cmd *exec.Cmd
	if e.Sandbox {
		bwrap := e.BwrapPath
		if bwrap == "" {
			bwrap = "bwrap"
		}
		args := append(sandboxArgs(e.Root, dir), "--", "bash", "-c", command)
		cmd = exec.CommandContext(runCtx, bwrap, args...)
	} else {
		cmd = exec.CommandContext(runCtx, "bash", "-c", command)
		cmd.Dir = dir
	}
	// Own process group, killed as a whole on timeout/abort (see
	// Cancel below). Without this, only the direct bash child died
	// and grandchildren lingered as orphans.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}

	legacy := outPath == ""
	filePath := outPath
	if legacy {
		tmp, err := os.CreateTemp("", "gmh-bash-*")
		if err == nil {
			filePath = tmp.Name()
			_ = tmp.Close()
			defer os.Remove(filePath)
		}
	}
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		// The file path is unusable (disk full, bad permissions):
		// fall back to the in-memory pipe path. The group kill
		// still bounds the timeout; a grandchild outliving the
		// direct child can still delay the wait there.
		return e.runBashCmdPipe(runCtx, cmd, timeout)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return "", -1, err
	}
	// Watch the output file: past the cap, kill the whole group.
	// A spewing command must not fill the disk while it runs out
	// its timeout.
	var capHit atomicBool
	stopWatch := make(chan struct{})
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-ticker.C:
				if st, err := f.Stat(); err == nil && st.Size() > OutputFileMaxBytes {
					capHit.set(true)
					if cmd.Process != nil {
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}
					return
				}
			}
		}
	}()
	runErr := cmd.Wait()
	close(stopWatch)
	_ = f.Close()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if legacy {
		data, _ := readFileHead(filePath, MaxOutBytes)
		s := truncate(string(data), MaxOutBytes)
		if capHit.get() {
			return s, -1, &outputCapError{bytes: int64(len(data))}
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return s, -1, &timeoutError{timeout}
		}
		if runErr != nil {
			return s, exitCode, fmt.Errorf("exit: %w", runErr)
		}
		return s, exitCode, nil
	}
	lines, bytes, fileCapped, head, tail, scanErr := scanOutputFile(filePath)
	if scanErr != nil {
		return "", exitCode, scanErr
	}
	s := FormatBashOutput(lines, bytes, fileCapped, head, tail, outPath, dir)
	if capHit.get() {
		return s, -1, &outputCapError{bytes: bytes}
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return s, -1, &timeoutError{timeout}
	}
	if runErr != nil {
		return s, exitCode, fmt.Errorf("exit: %w", runErr)
	}
	return s, exitCode, nil
}

// atomicBool is a mutex-free boolean for the cap watcher goroutine.
type atomicBool struct{ v int32 }

func (b *atomicBool) set(v bool) {
	if v {
		atomic.StoreInt32(&b.v, 1)
	} else {
		atomic.StoreInt32(&b.v, 0)
	}
}

func (b *atomicBool) get() bool { return atomic.LoadInt32(&b.v) != 0 }

// readFileHead returns up to max bytes from the start of the file.
func readFileHead(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(max)))
}

// runBashCmdPipe is the fallback when no output file can be used:
// the old in-memory behaviour (CombinedOutput capped at
// MaxOutBytes). The group kill still bounds the timeout, but a
// grandchild outliving the direct child can delay the wait —
// the pipe trap survives here, by necessity, not by choice.
func (e *Executor) runBashCmdPipe(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (string, int, error) {
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
// exit code. The job runs detached from the caller's ctx (the ctx
// argument is accepted but not used): a background job outlives its
// turn, and the turn ctx is cancelled after every turn.
func (e *Executor) BashBackground(ctx context.Context, a BashArgs) (*Job, error) {
	if reason := DeniedCommand(a.Command); reason != "" {
		e.audit(AuditEntry{Kind: "denied", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: reason})
		return nil, fmt.Errorf("denied by policy: %s (this is harm reduction, not a sandbox — see README)", reason)
	}
	dir, err := e.bashDir(a)
	if err != nil {
		e.audit(AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: err.Error()})
		return nil, err
	}
	timeout := e.bashTimeout(a)
	if a.Background && a.Timeout == 0 {
		// Explicit background with no per-call timeout: no
		// deadline. A dev server or watcher must outlive any cap —
		// the old 120s default killed it mid-session, which is
		// part of this bug: a never-ending job was given a
		// finite life. A per-call timeout, when given, is still
		// honored (and capped at 600s by bashTimeout).
		timeout = 0
	}
	outPath := e.nextOutputFile()
	job := &Job{Command: a.Command, OutputFile: outPath, Started: time.Now(), done: make(chan struct{})}
	e.registry().add(job)
	go func() {
		// Detached from the caller's ctx: a background job outlives
		// its turn by design, and the turn ctx is cancelled after
		// EVERY turn (runTurn calls cancel() after Handle, even on
		// a normally settled turn). Tied to that ctx, the exec
		// watcher fired cmd.Cancel at turn end — and the group kill
		// then killed the whole tree, so a dev server died the
		// moment its turn settled (proven live, and by
		// TestBashBackgroundSurvivesContextCancel). The per-call
		// timeout still works: runBashCmd derives it from this
		// base ctx. Trade-off: aborting a turn no longer kills
		// background jobs either (abort and turn-end cleanup cancel
		// the same ctx and cannot be told apart here); they run to
		// their timeout or completion, and their results surface
		// at the next turn via the Run-start re-attach.
		out, exitCode, err := e.runBashCmd(context.Background(), a.Command, timeout, outPath, dir)
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
