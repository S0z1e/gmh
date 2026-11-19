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
	"slices"
	"sync"
	"time"

	"minimalharness/internal/privatefile"
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
	Preview  *BashOutputPreview
}

// Job is one background bash execution.
type Job struct {
	// Invocation identity is immutable from construction.
	InvocationID string
	ToolCallID   string
	failureKey   string

	ID      string
	Command string
	Cwd     string
	// OutputFile is the per-call file holding the full stdout+stderr
	// ("" when OutputDir was unset); it grows while the job runs.
	OutputFile  string
	Started     time.Time
	cancel      context.CancelFunc
	closeCancel context.CancelFunc
	stopParent  func() bool

	mu         sync.Mutex
	done       chan struct{}
	result     JobResult
	finished   bool
	finishedAt time.Time
	registry   *JobRegistry
	// A later turn must not deliver a recorded result twice.
	delivered bool
	// A requested stop remains a delivery barrier even if context compaction
	// no longer contains its original acknowledgement.
	stopRequested bool
	// reported is set once a bounded drain told the model the job is
	// still running: the next turn's drain must not wait on it again
	// (the turn would pay the bound on every turn while a server runs).
	reported bool
}

// Done closes when the command finishes (success, failure or timeout).
func (j *Job) Done() <-chan struct{} { return j.done }

// FailureKey is the immutable effective invocation captured when this job began.
func (j *Job) FailureKey() string { return j.failureKey }

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
	if j.stopParent != nil {
		j.stopParent()
		j.stopParent = nil
	}
	j.result = r
	j.finished = true
	j.finishedAt = time.Now()
	registry, delivered := j.registry, j.delivered
	j.mu.Unlock()
	if delivered && registry != nil {
		registry.trimDelivered()
	}
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
	if j.delivered {
		j.mu.Unlock()
		return
	}
	j.delivered = true
	registry, finished := j.registry, j.finished
	j.mu.Unlock()
	// Registry operations lock jobs while holding the registry lock.
	// Notify the registry only after releasing the job lock.
	if finished && registry != nil {
		registry.trimDelivered()
	}
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
	j.mu.Lock()
	j.registry = r
	j.mu.Unlock()
	r.jobs = append(r.jobs, j)
	r.evictDelivered()
}

const recentJobsLimit = 50

func (r *JobRegistry) trimDelivered() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictDelivered()
}

// evictDelivered runs with the registry locked. A result can be finished
// without being delivered when the turn ended or another session was selected.
func (r *JobRegistry) evictDelivered() {
	remaining := len(r.jobs) - recentJobsLimit
	if remaining <= 0 {
		return
	}
	r.jobs = slices.DeleteFunc(r.jobs, func(job *Job) bool {
		if remaining == 0 {
			return false
		}
		job.mu.Lock()
		defer job.mu.Unlock()
		if !job.finished || !job.delivered {
			return false
		}
		job.registry = nil
		remaining--
		return true
	})
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
			j.mu.Lock()
			j.registry = nil
			j.mu.Unlock()
			r.jobs = slices.Delete(r.jobs, i, i+1)
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

// outputCapError marks a command stopped at the hard capture byte limit.
type outputCapError struct{ bytes int64 }

func (e *outputCapError) Error() string {
	return fmt.Sprintf("bash output exceeded %d bytes", OutputFileMaxBytes)
}

// bashOutputCapture owns output writes so the disk limit also applies to a
// child that outlives its shell. Both streams share one owned pipe copier;
// the shell exit starts a bounded drain independently of its exit status.
type bashOutputCapture struct {
	file    *os.File
	summary *bashOutputSink
	stop    func()
	bytes   int64
	capped  bool
	err     error
}

func (c *bashOutputCapture) Write(p []byte) (int, error) {
	keep := min(int64(len(p)), OutputFileMaxBytes-c.bytes)
	n, err := c.file.Write(p[:keep])
	if n > 0 {
		c.bytes += int64(n)
		_, _ = c.summary.Write(p[:n])
	}
	if err != nil {
		c.err = err
		c.stop()
		return n, err
	}
	if n < len(p) {
		c.capped = true
		c.stop()
		return n, &outputCapError{bytes: c.bytes}
	}
	return n, nil
}

// The shell and its captured stdout/stderr have separate lifetimes. A short
// bounded drain captures buffered output without waiting indefinitely for inherited
// handles; only background:true with a live shell retains captured output.
func (e *Executor) runBashCmd(ctx context.Context, command string, timeout time.Duration, outPath, dir string, preview *BashOutputPreview) (string, int, error) {
	var runCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
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
	cmd.Env = bashEnvironment(e.BashEnv)
	// Own process group, killed as a whole on timeout/abort (see
	// Cancel below). Without this, only the direct bash child died
	// and grandchildren lingered as orphans.
	configureProcess(cmd)
	var stopOnce sync.Once
	var stopErr error
	var processContextErr error
	var processMu sync.Mutex
	shellExited := false
	stopProcess := func(cause error) error {
		stopOnce.Do(func() {
			processContextErr = cause
			stopErr = stopBashProcessGroup(cmd)
		})
		return stopErr
	}
	cmd.Cancel = func() error {
		processMu.Lock()
		defer processMu.Unlock()
		if shellExited {
			return os.ErrProcessDone
		}
		return stopProcess(runCtx.Err())
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
	f, err := privatefile.Open(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return "", -1, fmt.Errorf("open bash output: %w", err)
	}
	capture := &bashOutputCapture{file: f, summary: newBashOutputSink(), stop: func() {
		// Capture errors still stop the inherited process group during
		// output drain, after the command deadline has been retired.
		_ = stopProcess(nil)
		cancel()
	}}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		return "", -1, fmt.Errorf("create bash output pipe: %w", err)
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		_ = f.Close()
		return "", -1, err
	}
	_ = writer.Close()
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(capture, reader)
		copied <- err
	}()
	// On supported platforms, observe shell exit without reaping it. Keeping
	// its PID reserved lets us stop the owned group before it can be reused.
	ownedGroup, observeErr := waitBashExit(cmd)
	var runErr error
	if observeErr != nil {
		// No Wait has run yet, so the PID is still ours. Fail closed if
		// exit observation is unavailable rather than abandoning children.
		_ = stopProcess(nil)
		ownedGroup = false
	}
	if !ownedGroup {
		runErr = cmd.Wait()
	}
	processMu.Lock()
	shellExited = true
	processMu.Unlock()
	// The deadline governs the shell, not the subsequent pipe drain. Its
	// watcher now returns ErrProcessDone without changing the shell outcome.
	cancel()
	timer := time.NewTimer(200 * time.Millisecond)
	var copyErr error
	captureClosed := false
	select {
	case copyErr = <-copied:
	case <-timer.C:
		select {
		case copyErr = <-copied:
		default:
			captureClosed = true
			_ = reader.Close()
			copyErr = <-copied
			if errors.Is(copyErr, os.ErrClosed) {
				copyErr = nil
			}
		}
	}
	timer.Stop()
	var cleanupErr error
	if ownedGroup {
		cleanupErr = stopProcess(nil)
		runErr = cmd.Wait()
		if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) {
			runErr = errors.Join(runErr, fmt.Errorf("stop remaining bash processes: %w", cleanupErr))
		}
	}
	if observeErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("observe bash exit: %w", observeErr))
	}
	_ = reader.Close()
	_ = capture.summary.Close()
	closeErr := f.Close()
	if capture.err != nil {
		runErr = capture.err
	} else if copyErr != nil {
		runErr = copyErr
	} else if runErr == nil {
		runErr = closeErr
	}
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	closureNotice := "\n[Output capture closed after the shell exited.]"
	if ownedGroup && (cleanupErr == nil || errors.Is(cleanupErr, os.ErrProcessDone)) {
		closureNotice = "\n[Output capture closed; remaining processes in the command group were stopped.]"
	}
	if legacy {
		data, _ := readFileHead(filePath, MaxOutBytes)
		s := truncate(string(data), MaxOutBytes)
		if preview != nil {
			*preview = bashPreview([]string{s}, []string{s}, true)
			if captureClosed {
				preview.Notice = closureNotice
			}
		}
		if captureClosed {
			s += closureNotice
		}
		if capture.capped {
			return s, -1, &outputCapError{bytes: int64(len(data))}
		}
		if capture.err != nil {
			return s, exitCode, fmt.Errorf("capture bash output: %w", capture.err)
		}
		if processContextErr == context.DeadlineExceeded {
			return s, -1, &timeoutError{timeout}
		}
		if processContextErr != nil {
			return s, -1, processContextErr
		}
		if ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrExecutorClosed) {
			return s, -1, ctx.Err()
		}
		if runErr != nil {
			return s, exitCode, fmt.Errorf("exit: %w", runErr)
		}
		return s, exitCode, nil
	}
	lines, bytes, fileCapped, head, tail := capture.summary.snapshot()
	if preview != nil {
		*preview = bashPreview(head, tail, lines <= len(head))
		if captureClosed {
			preview.Notice = closureNotice
		}
	}
	s := FormatBashOutput(lines, bytes, fileCapped || capture.capped, head, tail, outPath, dir)
	if captureClosed {
		s += closureNotice + "\n[Use background:true with a foreground server command for a long-lived service.]"
	}
	if capture.capped {
		return s, -1, &outputCapError{bytes: bytes}
	}
	if capture.err != nil {
		return s, exitCode, fmt.Errorf("capture bash output: %w", capture.err)
	}
	if processContextErr == context.DeadlineExceeded {
		return s, -1, &timeoutError{timeout}
	}
	if processContextErr != nil {
		return s, -1, processContextErr
	}
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrExecutorClosed) {
		return s, -1, ctx.Err()
	}
	if runErr != nil {
		return s, exitCode, fmt.Errorf("exit: %w", runErr)
	}
	return s, exitCode, nil
}

// readFileHead returns up to max bytes from the start of the file.
func readFileHead(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(max)))
}

// BashBackground starts a session-owned job. Cancellation of the turn does
// not stop a job after the loop has handed it to the session.
func (e *Executor) BashBackground(ctx context.Context, a BashArgs) (*Job, error) {
	return e.startBashJob(ctx, a, false)
}

// BashForeground starts a cancellable command that can later be detached
// when the foreground threshold expires.
func (e *Executor) BashForeground(ctx context.Context, a BashArgs) (*Job, error) {
	return e.startBashJob(ctx, a, true)
}

func (e *Executor) startBashJob(ctx context.Context, a BashArgs, foreground bool) (*Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reason := DeniedCommand(a.Command); reason != "" {
		e.audit(ctx, AuditEntry{Kind: "denied", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: reason})
		return nil, &PolicyError{Reason: reason}
	}
	dir, err := e.bashDir(a)
	if err != nil {
		e.audit(ctx, AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: err.Error()})
		return nil, err
	}
	timeout := e.bashTimeout(a)
	if a.Background && a.Timeout == 0 {
		timeout = 0
	}
	outPath := e.nextOutputFile()
	runCtx, cancelCause := context.WithCancelCause(context.Background())
	cancel := func() { cancelCause(context.Canceled) }
	identity := invocationFromContext(ctx)
	key, _ := ctx.Value(failureKeyContext{}).(string)
	job := &Job{InvocationID: identity.id, ToolCallID: identity.callID, failureKey: key, Command: a.Command, Cwd: dir, OutputFile: outPath, Started: time.Now(), done: make(chan struct{}), cancel: cancel,
		closeCancel: func() { cancelCause(ErrExecutorClosed) }}
	if foreground {
		job.stopParent = context.AfterFunc(ctx, cancel)
	}
	// Capture the audit destination: a later session switch must not redirect it.
	audit := e.Audit
	e.jobsMu.Lock()
	if e.jobsClosed {
		e.jobsMu.Unlock()
		job.Cancel()
		job.Detach()
		return nil, ErrExecutorClosed
	}
	if e.jobs == nil {
		e.jobs = &JobRegistry{}
	}
	e.jobs.add(job)
	e.jobsMu.Unlock()
	// Close the race with an abort that ran its registry snapshot just
	// before this job was registered.
	if ctx.Err() != nil {
		job.Cancel()
	}
	go func() {
		defer cancel()
		preview := &BashOutputPreview{}
		commandCtx, finish, err := e.beginBashRun(runCtx)
		out, code := "", -1
		if err == nil {
			defer finish()
			out, code, err = e.runBashCmd(commandCtx, a.Command, timeout, outPath, dir, preview)
		}
		if audit != nil {
			entry := AuditEntry{InvocationID: job.InvocationID, ToolCallID: job.ToolCallID, TS: time.Now(), Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: code}
			if err != nil {
				entry.Error = err.Error()
			}
			audit(entry)
		}
		job.finish(JobResult{Output: out, Err: err, ExitCode: code, Preview: preview})
	}()
	return job, nil
}

// Detach transfers cancellation ownership from the turn to the session.
func (j *Job) Detach() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopParent != nil {
		j.stopParent()
		j.stopParent = nil
	}
}

func (j *Job) Cancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stopRequested = true
	if j.cancel != nil {
		j.cancel()
	}
}

// StopRequested reports whether this live job has been explicitly cancelled.
func (j *Job) StopRequested() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stopRequested
}

// CancelJobs explicitly stops the current session's jobs. Turn cancellation
// deliberately does not call it: detached work belongs to the session.
func (e *Executor) CancelJobs() {
	e.SessionCanceller()()
}

// SessionCanceller captures the owning registry, so a delayed abort can never
// cancel commands from a subsequently selected session.
func (e *Executor) SessionCanceller() func() {
	r := e.registry()
	return func() {
		for _, job := range r.List() {
			job.Cancel()
		}
	}
}

// Jobs returns a snapshot of all background jobs for this Executor.
func (e *Executor) Jobs() []*Job { return e.registry().List() }

// HasRunningWorkspaceJobs includes retained sessions sharing this Executor's
// workspace, without exposing another session's job IDs or output.
func (e *Executor) HasRunningWorkspaceJobs() bool { return e.RunningWorkspaceJobs() > 0 }

// RunningWorkspaceJobs counts active jobs across current and retained sessions.
// Launches and session switches are serialized by the agent loop.
func (e *Executor) RunningWorkspaceJobs() int64 {
	e.jobsMu.Lock()
	registries := map[*JobRegistry]bool{}
	if e.jobs != nil {
		registries[e.jobs] = true
	}
	for _, registry := range e.sessionJobs {
		if registry != nil {
			registries[registry] = true
		}
	}
	e.jobsMu.Unlock()
	var running int64
	for registry := range registries {
		for _, job := range registry.List() {
			if _, done := job.Result(); !done {
				running++
			}
		}
	}
	return running
}

func (e *Executor) registry() *JobRegistry {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	if e.jobs == nil {
		e.jobs = &JobRegistry{}
	}
	return e.jobs
}

// SwitchSession retains each session's jobs without exposing them to another.
// Session switches happen between turns; job goroutines capture their owner.
func (e *Executor) SwitchSession(id string) {
	e.editMu.Lock()
	e.snapshots = nil
	e.editMu.Unlock()
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	if e.sessionJobs == nil {
		e.sessionJobs = map[string]*JobRegistry{}
	}
	if e.jobs != nil {
		e.sessionJobs[e.sessionID] = e.jobs
	}
	e.sessionID = id
	e.jobs = e.sessionJobs[id]
	if e.jobs == nil {
		e.jobs = &JobRegistry{}
		e.sessionJobs[id] = e.jobs
	}
}

func (e *Executor) RenameSession(oldID, newID string) {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	if registry := e.sessionJobs[oldID]; registry != nil {
		e.sessionJobs[newID] = registry
		delete(e.sessionJobs, oldID)
	}
	if e.sessionID == oldID {
		e.sessionID = newID
	}
}
