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
	"path/filepath"
	"slices"
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
	Output          string
	Err             error
	ExitCode        int
	ExitCodeUnknown bool
	Preview         *BashOutputPreview
}

// Job is one background bash execution.
type Job struct {
	// Invocation identity is immutable from construction.
	InvocationID string
	ToolCallID   string
	failureKey   string

	ID             string
	Command        string
	Cwd            string
	OutputID       string
	OutputReceipt  string
	PID            int
	PGID           int
	ProcessStarted string
	JobToken       string
	groupProof     processGroupProof
	proofFDMu      sync.Mutex
	Adopted        bool
	RecoveryNote   string
	// OutputFile is the per-call file holding the full stdout+stderr
	// ("" when OutputDir was unset); it grows while the job runs.
	OutputFile   string
	Started      time.Time
	Deadline     time.Time
	Unlimited    bool
	WaitError    string
	cancel       context.CancelFunc
	closeCancel  context.CancelFunc
	closeStop    func() error
	stopParent   func() bool
	stopDeadline func()
	monitorDone  chan struct{}

	jobsDir   string
	outputDir string
	exec      *Executor
	audit     func(AuditEntry)
	persistMu sync.Mutex

	mu                sync.Mutex
	done              chan struct{}
	result            JobResult
	finished          bool
	unknown           bool
	monitorFailed     bool
	cleanupPending    bool
	expiredOnAdoption bool
	retained          bool
	forgotten         bool
	finishedAt        time.Time
	registry          *JobRegistry
	// A later turn must not deliver a recorded result twice.
	delivered bool
	// adoptionNoticed ensures an adopted running job sends at most one
	// post-restart advisory notice to the model.
	adoptionNoticed bool
	// A requested stop remains a delivery barrier even if context compaction
	// no longer contains its original acknowledgement.
	stopRequested bool
	// reported is set once a bounded drain told the model the job is
	// still running: the next turn's drain must not wait on it again
	// (the turn would pay the bound on every turn while a server runs).
	reported bool
}

// JobSnapshot is an atomic, race-free point-in-time copy of a Job's state.
type JobSnapshot struct {
	ID              string
	Started         time.Time
	Deadline        time.Time
	Unlimited       bool
	InvocationID    string
	ToolCallID      string
	Command         string
	Cwd             string
	OutputFile      string
	OutputID        string
	OutputReceipt   string
	Identity        ProcessIdentityInfo
	Adopted         bool
	RecoveryNote    string
	WaitError       string
	Status          JobStatus
	FinishedAt      time.Time
	Delivered       bool
	AdoptionNoticed bool
	Reported        bool
	StopRequested   bool
	Result          JobResult
	HasOutputDir    bool
}

// Snapshot returns an atomic thread-safe copy of j's current state.
func (j *Job) Snapshot() JobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	status := JobRunning
	if j.finished {
		status = JobDone
	} else if j.unknown {
		status = JobUnknown
	}
	return JobSnapshot{
		ID:              j.ID,
		Started:         j.Started,
		Deadline:        j.Deadline,
		Unlimited:       j.Unlimited,
		InvocationID:    j.InvocationID,
		ToolCallID:      j.ToolCallID,
		Command:         j.Command,
		Cwd:             j.Cwd,
		OutputFile:      j.OutputFile,
		OutputID:        j.OutputID,
		OutputReceipt:   j.OutputReceipt,
		Identity:        ProcessIdentityInfo{PID: j.PID, PGID: j.PGID, StartToken: j.ProcessStarted},
		Adopted:         j.Adopted,
		RecoveryNote:    j.RecoveryNote,
		WaitError:       j.WaitError,
		Status:          status,
		FinishedAt:      j.finishedAt,
		Delivered:       j.delivered,
		AdoptionNoticed: j.adoptionNoticed,
		Reported:        j.reported,
		StopRequested:   j.stopRequested,
		Result:          j.result,
		HasOutputDir:    j.outputDir != "",
	}
}

// Done closes when the command finishes (success, failure or timeout) or is
// resolved to unknown status after a restart.
func (j *Job) Done() <-chan struct{} { return j.done }

// FailureKey is the immutable effective invocation captured when this job began.
func (j *Job) FailureKey() string { return j.failureKey }

// FinishedAt reports when the job finished; ok is false while running or unknown.
func (j *Job) FinishedAt() (time.Time, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finishedAt, j.finished
}

// Status reports running, done, or unknown.
func (j *Job) Status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished {
		return JobDone
	}
	if j.unknown {
		return JobUnknown
	}
	return JobRunning
}

// RecoveryNoteText returns the post-crash recovery note or reason if any.
func (j *Job) RecoveryNoteText() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.RecoveryNote
}

// Result returns the outcome once finished; ok is false while running or unknown.
func (j *Job) Result() (JobResult, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result, j.finished
}

// UnknownResult returns the diagnostic result recorded for a JobUnknown job.
func (j *Job) UnknownResult() (JobResult, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result, j.unknown
}

// Elapsed is how long the job has been running, or how long it ran
// for once finished or marked unknown.
func (j *Job) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if (j.finished || j.unknown) && !j.finishedAt.IsZero() {
		return j.finishedAt.Sub(j.Started)
	}
	return time.Since(j.Started)
}

func (j *Job) snapshotForPersist() (PersistedJob, string, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	status := JobRunning
	if j.finished {
		status = JobDone
	} else if j.unknown {
		status = JobUnknown
	}
	var exitCode *int
	if j.finished && !j.result.ExitCodeUnknown {
		code := j.result.ExitCode
		exitCode = &code
	}
	var errMsg string
	if j.result.Err != nil {
		errMsg = j.result.Err.Error()
	}
	var persistedProof *PersistedGroupProof
	if status == JobRunning && (j.Adopted || j.cleanupPending || !j.groupProof.VerifiedAt.IsZero()) {
		persistedProof = persistedGroupProofFromProof(j.groupProof, j.cleanupPending || !j.groupProof.ExitObservedAt.IsZero())
	}
	return PersistedJob{
		ID:              j.ID,
		Started:         j.Started,
		Deadline:        j.Deadline,
		Unlimited:       j.Unlimited,
		InvocationID:    j.InvocationID,
		ToolCallID:      j.ToolCallID,
		Command:         j.Command,
		Cwd:             j.Cwd,
		OutputFile:      j.OutputFile,
		OutputID:        j.OutputID,
		OutputReceipt:   j.OutputReceipt,
		PID:             j.PID,
		PGID:            j.PGID,
		ProcessStarted:  j.ProcessStarted,
		JobToken:        j.JobToken,
		GroupProof:      persistedProof,
		Status:          status,
		ExitCode:        exitCode,
		ExitCodeUnknown: j.result.ExitCodeUnknown,
		Error:           errMsg,
		FinishedAt:      j.finishedAt,
		Delivered:       j.delivered,
		StopRequested:   j.stopRequested,
		AdoptionNoticed: j.adoptionNoticed,
		UnknownNoticed:  status == JobUnknown && j.delivered,
		RecoveryNote:    j.RecoveryNote,
	}, j.jobsDir, j.retained && !j.forgotten
}

func (j *Job) persistedSnapshot() PersistedJob {
	snap, _, _ := j.snapshotForPersist()
	return snap
}

func (j *Job) persist() {
	j.persistMu.Lock()
	defer j.persistMu.Unlock()
	snap, jobsDir, shouldPersist := j.snapshotForPersist()
	if !shouldPersist || jobsDir == "" || snap.ID == "" {
		return
	}
	_ = savePersistedJob(jobsDir, snap)
}

func (j *Job) removePersisted() {
	j.persistMu.Lock()
	defer j.persistMu.Unlock()
	j.mu.Lock()
	j.retained = false
	j.forgotten = true
	jobsDir, id, started := j.jobsDir, j.ID, j.Started
	j.mu.Unlock()
	if jobsDir != "" && id != "" {
		removePersistedJob(jobsDir, id, started)
	}
}

func (j *Job) markUnknown(reason, outputSummary string, preview *BashOutputPreview) {
	j.mu.Lock()
	if j.finished || j.unknown {
		j.mu.Unlock()
		return
	}
	if j.stopParent != nil {
		j.stopParent()
		j.stopParent = nil
	}
	if j.stopDeadline != nil {
		j.stopDeadline()
		j.stopDeadline = nil
	}
	j.unknown = true
	j.RecoveryNote = reason
	j.finishedAt = time.Now()
	j.result = JobResult{
		Output:          outputSummary,
		Err:             errors.New(reason),
		ExitCode:        -1,
		ExitCodeUnknown: true,
		Preview:         preview,
	}
	j.mu.Unlock()
	j.closeProofFDs()
	j.persist()
	close(j.done)
}

func (j *Job) finish(r JobResult) {
	j.finishCore(r, nil, nil)
}

func (j *Job) finishCore(r JobResult, prePersist func(), postPersist func()) bool {
	j.mu.Lock()
	if j.finished || j.unknown {
		j.mu.Unlock()
		return false
	}
	if j.stopParent != nil {
		j.stopParent()
		j.stopParent = nil
	}
	if j.stopDeadline != nil {
		j.stopDeadline()
		j.stopDeadline = nil
	}
	j.result = r
	j.finished = true
	j.cleanupPending = false
	j.finishedAt = time.Now()
	registry, delivered := j.registry, j.delivered
	j.mu.Unlock()
	j.closeProofFDs()
	if prePersist != nil {
		prePersist()
	}
	if delivered && registry != nil {
		registry.trimDelivered()
	}
	j.persist()
	if postPersist != nil {
		postPersist()
	}
	close(j.done)
	return true
}

func (j *Job) closeProofFDs() {
	j.proofFDMu.Lock()
	defer j.proofFDMu.Unlock()
	closeVerifiedGroupProof(&j.groupProof)
}

// Delivered reports whether the job's terminal outcome (or unknown notice) was
// already fed back to the model.
func (j *Job) Delivered() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.delivered
}

// MarkDelivered records that the terminal result or unknown notice was fed back
// to the model.
func (j *Job) MarkDelivered() {
	j.mu.Lock()
	if j.delivered {
		j.mu.Unlock()
		return
	}
	j.delivered = true
	if j.OutputFile != "" && j.outputDir != "" && j.OutputReceipt == "" {
		j.OutputReceipt = filepath.Base(j.OutputFile)
	}
	registry, settled := j.registry, j.finished || j.unknown
	j.mu.Unlock()
	// Registry operations lock jobs while holding the registry lock.
	// Notify the registry only after releasing the job lock.
	if settled && registry != nil {
		registry.trimDelivered()
	}
	j.persist()
}

// AdoptionNoticed reports whether the model was already told this adopted job
// is running after restart.
func (j *Job) AdoptionNoticed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.adoptionNoticed
}

// MarkAdoptionNoticed records that the post-restart running notice was
// delivered for this adopted job.
func (j *Job) MarkAdoptionNoticed() {
	j.mu.Lock()
	if j.adoptionNoticed {
		j.mu.Unlock()
		return
	}
	j.adoptionNoticed = true
	j.mu.Unlock()
	j.persist()
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
	mu       sync.Mutex
	jobs     []*Job
	next     int
	abortGen uint64
}

func (r *JobRegistry) abortGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortGen
}

func (r *JobRegistry) prepareCancelAll() func() {
	r.mu.Lock()
	r.abortGen++
	jobs := make([]*Job, len(r.jobs))
	copy(jobs, r.jobs)
	r.mu.Unlock()
	return func() {
		for _, job := range jobs {
			job.Cancel()
		}
	}
}

func (r *JobRegistry) cancelAll() {
	r.prepareCancelAll()()
}

func (r *JobRegistry) add(j *Job) {
	r.addWithSeq(j, 0)
}

func (r *JobRegistry) addWithSeq(j *Job, seq int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq > 0 {
		if seq > r.next {
			r.next = seq
		}
		j.ID = fmt.Sprintf("bg-%d", seq)
	} else {
		r.next++
		j.ID = fmt.Sprintf("bg-%d", r.next)
	}
	j.mu.Lock()
	j.registry = r
	j.mu.Unlock()
	r.jobs = append(r.jobs, j)
	r.evictDelivered()
}

func (r *JobRegistry) addPreserved(j *Job) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq := jobSequenceNumber(j.ID); seq > r.next {
		r.next = seq
	}
	j.mu.Lock()
	j.registry = r
	j.mu.Unlock()
	r.jobs = append(r.jobs, j)
	r.evictDelivered()
	return r.abortGen
}

func (r *JobRegistry) listLocked() []*Job {
	out := make([]*Job, len(r.jobs))
	copy(out, r.jobs)
	return out
}

const (
	recentJobsLimit        = 50
	staleUndeliveredJobAge = time.Hour
)

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
		if (!job.finished && !job.unknown) || !job.delivered {
			return false
		}
		job.registry = nil
		remaining--
		return true
	})
	// Also reclaim abandoned finished jobs once the registry exceeds 2x the
	// normal cap so long-running processes do not leak memory indefinitely.
	overflow := len(r.jobs) - recentJobsLimit*2
	if overflow <= 0 {
		return
	}
	cutoff := time.Now().Add(-staleUndeliveredJobAge)
	r.jobs = slices.DeleteFunc(r.jobs, func(job *Job) bool {
		if overflow == 0 {
			return false
		}
		job.mu.Lock()
		defer job.mu.Unlock()
		if (!job.finished && !job.unknown) || job.finishedAt.After(cutoff) {
			return false
		}
		job.registry = nil
		overflow--
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

// ForgetJob removes a job from this Executor's list and discards any transient
// persisted sidecar record (see forget).
func (e *Executor) ForgetJob(j *Job) {
	e.registry().forget(j)
	if j != nil {
		j.removePersisted()
	}
}

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
	file    io.Writer
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

const bashOutputDrainTimeout = 200 * time.Millisecond

type startedBashProcess struct {
	cmd               *exec.Cmd
	cancel            context.CancelFunc
	stopProcess       func(error) error
	processContextErr *error
	shellExited       *bool
	processMu         *sync.Mutex
	reader            *os.File
	capture           *bashOutputCapture
	copied            <-chan error
	pid               int
	pgid              int
	startToken        string
	info              processInfo
}

func (e *Executor) startBashProcess(ctx context.Context, command string, timeout time.Duration, output *bashOutputFile, dir string, extraEnv ...string) (*startedBashProcess, error) {
	var runCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
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
	env := bashEnvironment(e.BashEnv)
	if len(extraEnv) > 0 {
		env = append(append([]string(nil), env...), extraEnv...)
	}
	cmd.Env = env
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

	capture := &bashOutputCapture{file: output, summary: newBashOutputSink(), stop: func() {
		// Capture errors still stop the inherited process group during
		// output drain, after the command deadline has been retired.
		_ = stopProcess(nil)
		cancel()
	}}
	reader, writer, err := os.Pipe()
	if err != nil {
		cancel()
		output.Close()
		return nil, fmt.Errorf("create bash output pipe: %w", err)
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if reviewed := bashDirectoryFromContext(ctx); reviewed != nil {
		err = reviewed.check(dir)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		cancel()
		output.Close()
		return nil, err
	}
	pid := 0
	pgid := 0
	var startToken string
	var info processInfo
	if cmd.Process != nil {
		pid = cmd.Process.Pid
		pgid = processGroupID(pid)
		info = inspectProcess(pid)
		if info.PGID > 0 {
			pgid = info.PGID
		}
		startToken = info.StartToken
	}
	_ = writer.Close()
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(capture, reader)
		copied <- err
	}()
	return &startedBashProcess{
		cmd:               cmd,
		cancel:            cancel,
		stopProcess:       stopProcess,
		processContextErr: &processContextErr,
		shellExited:       &shellExited,
		processMu:         &processMu,
		reader:            reader,
		capture:           capture,
		copied:            copied,
		pid:               pid,
		pgid:              pgid,
		startToken:        startToken,
		info:              info,
	}, nil
}

// The shell and its captured stdout/stderr have separate lifetimes. A short
// bounded drain captures buffered output without waiting indefinitely for inherited
// handles; only background:true with a live shell retains captured output.
func (e *Executor) runBashCmd(ctx context.Context, command string, timeout, drainTimeout time.Duration, output *bashOutputFile, dir string, preview *BashOutputPreview) (string, int, error) {
	proc, err := e.startBashProcess(ctx, command, timeout, output, dir)
	if err != nil {
		return "", -1, err
	}
	return e.waitStartedBashCmd(ctx, proc, timeout, drainTimeout, output, dir, preview)
}

func (e *Executor) waitStartedBashCmd(ctx context.Context, proc *startedBashProcess, timeout, drainTimeout time.Duration, output *bashOutputFile, dir string, preview *BashOutputPreview) (string, int, error) {
	defer output.Close()
	defer proc.cancel()
	cmd := proc.cmd
	outPath := output.path
	legacy := output.legacy
	stopProcess := proc.stopProcess
	processMu := proc.processMu
	reader := proc.reader
	capture := proc.capture
	copied := proc.copied
	cancel := proc.cancel

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
	*proc.shellExited = true
	processMu.Unlock()
	// The deadline governs the shell, not the subsequent pipe drain. Its
	// watcher now returns ErrProcessDone without changing the shell outcome.
	cancel()
	timer := time.NewTimer(drainTimeout)
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
	closeErr := output.file.Close()
	processContextErr := *proc.processContextErr
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
	var quota *OutputQuotaError
	quotaNotice := ""
	if errors.As(capture.err, &quota) {
		quotaNotice = "\n[" + quota.Error() + "; command stopped.]"
	}
	if legacy {
		data, _ := readFileHead(outPath, MaxOutBytes)
		s := truncate(string(data), MaxOutBytes)
		if preview != nil {
			*preview = bashPreview([]string{s}, []string{s}, true)
			if captureClosed {
				preview.Notice = closureNotice
			}
			if quotaNotice != "" {
				preview.Notice = quotaNotice
			}
		}
		if captureClosed {
			s += closureNotice
		}
		s += quotaNotice
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
		if quotaNotice != "" {
			preview.Notice = quotaNotice
		}
	}
	s := FormatBashOutput(lines, bytes, fileCapped || capture.capped, head, tail, outPath, dir)
	if captureClosed {
		s += closureNotice + "\n[Use background:true with a foreground server command for a long-lived service.]"
	}
	s += quotaNotice
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
	runCtx, cancelCause := context.WithCancelCause(context.Background())
	cancel := func() { cancelCause(context.Canceled) }
	commandCtx, finish, err := e.beginBashRun(runCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	if reviewed := bashDirectoryFromContext(ctx); reviewed != nil {
		commandCtx = WithBashDirectory(commandCtx, reviewed)
	}
	transferred := false
	defer func() {
		if !transferred {
			finish()
			cancel()
		}
	}()
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
		timeout = e.backgroundMaxLifetime()
	}
	output, err := e.newBashOutput(commandCtx)
	if err != nil {
		e.audit(ctx, AuditEntry{Kind: "bash", Tool: "bash", Args: truncate(a.Command, 500), ExitCode: -1, Error: err.Error()})
		return nil, err
	}
	identity := invocationFromContext(ctx)
	key, _ := ctx.Value(failureKeyContext{}).(string)
	jobsDir := e.JobsDirPath()
	started := time.Now()
	var deadline time.Time
	unlimited := false
	if timeout > 0 {
		deadline = started.Add(timeout)
	} else {
		unlimited = true
	}
	jobToken := randomHexToken(16)
	job := &Job{
		InvocationID: identity.id,
		ToolCallID:   identity.callID,
		failureKey:   key,
		Command:      a.Command,
		Cwd:          dir,
		OutputID:     e.OutputID,
		OutputFile:   output.publicPath(),
		JobToken:     jobToken,
		jobsDir:      jobsDir,
		outputDir:    e.OutputDir,
		retained:     !foreground || jobsDir != "",
		Started:      started,
		Deadline:     deadline,
		Unlimited:    unlimited,
		done:         make(chan struct{}),
		cancel:       cancel,
		closeCancel:  func() { cancelCause(ErrExecutorClosed) },
	}
	if foreground {
		job.stopParent = context.AfterFunc(ctx, cancel)
	}
	// Capture the audit destination: a later session switch must not redirect it.
	audit := e.Audit
	e.jobsMu.Lock()
	if e.jobsClosed {
		e.jobsMu.Unlock()
		job.mu.Lock()
		job.retained = false
		job.mu.Unlock()
		job.Cancel()
		job.Detach()
		output.Close()
		return nil, ErrExecutorClosed
	}
	if e.jobs == nil {
		e.jobs = &JobRegistry{}
	}
	reg := e.jobs
	startAbortGen := reg.abortGeneration()
	e.nextJobSeq++
	seq := e.nextJobSeq
	e.jobsMu.Unlock()

	job.ID = fmt.Sprintf("bg-%d", seq)
	// Close the race with an abort/cancel that already marked ctx done before
	// starting the OS process, while populating PID/PGID/StartToken before the
	// job is published into the registry.
	if ctx.Err() != nil {
		job.mu.Lock()
		job.stopRequested = true
		job.mu.Unlock()
		cancel()
	}
	proc, startErr := e.startBashProcess(commandCtx, a.Command, timeout, output, dir, jobTokenEnvVar+"="+jobToken)
	if startErr == nil {
		job.mu.Lock()
		job.PID = proc.pid
		job.PGID = proc.pgid
		job.ProcessStarted = proc.startToken
		id := ProcessIdentityInfo{PID: proc.pid, PGID: proc.pgid, StartToken: proc.startToken}
		if id.Valid() {
			job.groupProof = processGroupProof{
				Leader:       id,
				UID:          proc.info.UID,
				HasUID:       proc.info.HasUID,
				SID:          proc.info.SID,
				Sess:         proc.info.Sess,
				JobToken:     jobToken,
				OutputFile:   job.OutputFile,
				KnownMembers: map[int]string{id.PID: id.StartToken},
				VerifiedAt:   time.Now(),
			}
		}
		job.mu.Unlock()
	}
	if e.beforeJobPublish != nil {
		e.beforeJobPublish()
	}

	e.jobsMu.Lock()
	if e.jobsClosed {
		e.jobsMu.Unlock()
		job.mu.Lock()
		job.retained = false
		job.mu.Unlock()
		if startErr == nil {
			cancel()
			go func() {
				defer finish()
				_, _, _ = e.waitStartedBashCmd(commandCtx, proc, timeout, bashOutputDrainTimeout, output, dir, nil)
			}()
		} else {
			cancel()
			output.Close()
			finish()
		}
		transferred = true
		return nil, ErrExecutorClosed
	}
	// Publish into the same registry captured before fork and check whether an
	// abort cancelled that registry after startAbortGen was sampled. If so,
	// cancel commandCtx here so in-progress launches cannot escape abort.
	abortObserved := reg.addPreserved(job) != startAbortGen || ctx.Err() != nil
	if abortObserved {
		job.mu.Lock()
		job.stopRequested = true
		job.mu.Unlock()
	}
	e.jobsMu.Unlock()
	if abortObserved {
		cancel()
	}
	job.persist()
	transferred = true
	go func() {
		defer cancel()
		defer finish()
		preview := &BashOutputPreview{}
		var out string
		var code int
		var err error
		if startErr != nil {
			code = -1
			err = startErr
		} else {
			out, code, err = e.waitStartedBashCmd(commandCtx, proc, timeout, bashOutputDrainTimeout, output, dir, preview)
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
	if j.stopParent != nil {
		j.stopParent()
		j.stopParent = nil
	}
	promote := !j.retained && !j.forgotten && j.ID != ""
	if promote {
		j.retained = true
	}
	j.mu.Unlock()
	if promote {
		j.persist()
	}
}

func (j *Job) Cancel() {
	_ = j.CancelErr()
}

// CancelErr explicitly stops the job's OS process group and returns any
// non-benign OS signalling error (such as EPERM or ErrProcessIdentityMismatch).
func (j *Job) CancelErr() error {
	j.mu.Lock()
	if j.finished || j.unknown {
		j.mu.Unlock()
		return nil
	}
	j.stopRequested = true
	adopted := j.Adopted
	cancel := j.cancel
	j.mu.Unlock()
	if adopted {
		err := j.stopAdoptedProcess(context.Canceled, false)
		if errors.Is(err, ErrProcessIdentityMismatch) && cancel != nil {
			cancel()
		}
		return err
	}
	if cancel != nil {
		cancel()
	}
	j.persist()
	return nil
}

func (j *Job) recordStopError(err error) {
	if err == nil {
		return
	}
	j.mu.Lock()
	if !j.finished && !j.unknown {
		j.monitorFailed = true
		if !j.groupProof.ExitObservedAt.IsZero() {
			j.cleanupPending = true
		}
		j.WaitError = err.Error()
		j.RecoveryNote = fmt.Sprintf("adopted live OS process PID %d (pgid %d); stop error (%v) — job remains stoppable; %s",
			j.PID, j.PGID, err, pipeCaptureEndedNote(j.OutputFile))
	}
	j.mu.Unlock()
	j.persist()
}

func (j *Job) snapshotGroupProofLocked() processGroupProof {
	proof := cloneGroupProof(j.groupProof)
	proof.verifiedFDs = j.groupProof.verifiedFDs
	if proof.Leader == (ProcessIdentityInfo{}) {
		proof.Leader = ProcessIdentityInfo{PID: j.PID, PGID: j.PGID, StartToken: j.ProcessStarted}
	}
	if proof.JobToken == "" {
		proof.JobToken = j.JobToken
	}
	if proof.OutputFile == "" {
		proof.OutputFile = j.OutputFile
	}
	return proof
}

func (j *Job) handleAdoptedLeaderIdentityMismatch(outFile, cwd string) bool {
	j.proofFDMu.Lock()
	j.mu.Lock()
	proof := j.snapshotGroupProofLocked()
	j.mu.Unlock()
	hasProof := hasIndependentGroupProof(proof)
	j.proofFDMu.Unlock()
	if hasProof {
		return true
	}
	summary, preview, _, _ := summarizeOutputFile(outFile, cwd)
	j.markUnknown("process identity changed unexpectedly during observation (possible PID reuse)", summary, preview)
	return false
}

func populateGroupProofIdentity(proof *processGroupProof, info processInfo) {
	if proof == nil || !info.Alive {
		return
	}
	if !proof.HasUID && info.HasUID {
		proof.UID = info.UID
		proof.HasUID = true
	}
	if proof.SID <= 0 && info.SID > 0 {
		proof.SID = info.SID
	}
	if proof.Sess == 0 && info.Sess != 0 {
		proof.Sess = info.Sess
	}
}

func (j *Job) refreshVerifiedGroupMembersLocked() processGroupProof {
	j.mu.Lock()
	if j.finished || j.unknown {
		proof := j.snapshotGroupProofLocked()
		j.mu.Unlock()
		return proof
	}
	proof := j.snapshotGroupProofLocked()
	j.mu.Unlock()

	if proof.Leader.Valid() && (!proof.HasUID || proof.Sess == 0 || proof.SID <= 0) {
		if linfo, lerr := inspectProcessErr(proof.Leader.PID); lerr == nil && proof.Leader.Matches(linfo) {
			populateGroupProofIdentity(&proof, linfo)
		}
	}
	captureVerifiedGroupProof(&proof)

	j.mu.Lock()
	prevMembers := len(j.groupProof.KnownMembers)
	prevPipes := len(j.groupProof.LeaderPipeTargets)
	prevVerified := !j.groupProof.VerifiedAt.IsZero()
	j.groupProof.KnownMembers = mergeGroupMembers(j.groupProof.KnownMembers, proof.KnownMembers)
	j.groupProof.LeaderPipeTargets = mergePipeTargets(j.groupProof.LeaderPipeTargets, proof.LeaderPipeTargets)
	if !j.groupProof.HasUID && proof.HasUID {
		j.groupProof.UID = proof.UID
		j.groupProof.HasUID = true
	}
	if j.groupProof.SID <= 0 && proof.SID > 0 {
		j.groupProof.SID = proof.SID
	}
	if j.groupProof.Sess == 0 && proof.Sess != 0 {
		j.groupProof.Sess = proof.Sess
	}
	if !proof.VerifiedAt.IsZero() {
		j.groupProof.VerifiedAt = proof.VerifiedAt
	}
	j.groupProof.verifiedFDs = proof.verifiedFDs
	var staleProof processGroupProof
	if j.groupProof.jobHandle == 0 && proof.jobHandle != 0 {
		j.groupProof.jobHandle = proof.jobHandle
	} else if proof.jobHandle != 0 && proof.jobHandle != j.groupProof.jobHandle {
		staleProof.jobHandle = proof.jobHandle
	}
	changed := (!prevVerified && !j.groupProof.VerifiedAt.IsZero()) || len(j.groupProof.KnownMembers) > prevMembers || len(j.groupProof.LeaderPipeTargets) > prevPipes
	out := j.snapshotGroupProofLocked()
	j.mu.Unlock()
	if staleProof.jobHandle != 0 {
		closeVerifiedGroupProof(&staleProof)
	}
	if changed {
		j.persist()
	}
	return out
}

func (j *Job) runStopGroupAfterExit(proof *processGroupProof) error {
	if j.exec != nil && j.exec.stopAdoptedGroupAfterExit != nil {
		return j.exec.stopAdoptedGroupAfterExit(*proof)
	}
	return stopAdoptedProcessGroupAfterExitTrack(proof)
}

func formatAdoptedSummaryPreview(outFile, cwd, emptyMsg, fileNote string) (string, *BashOutputPreview) {
	summary, preview, _, hasFile := summarizeOutputFile(outFile, cwd)
	if !hasFile {
		summary = emptyMsg
	} else {
		summary += "\n[" + fileNote + "]"
	}
	if preview != nil {
		note := "[" + fileNote + "]"
		if preview.Notice != "" {
			preview.Notice += "\n" + note
		} else {
			preview.Notice = note
		}
	}
	return summary, preview
}

func (j *Job) finishAdoptedWithAudit(r JobResult, auditFn func(AuditEntry), auditCode int, auditErr error) bool {
	j.mu.Lock()
	invID, callID, cmd := j.InvocationID, j.ToolCallID, j.Command
	execRef, outFile, outputDir := j.exec, j.OutputFile, j.outputDir
	j.mu.Unlock()

	return j.finishCore(r, func() {
		if execRef != nil && outFile != "" && outputDir != "" {
			execRef.unmarkActiveOutput(outputDir, outFile)
		}
	}, func() {
		if auditFn != nil {
			entry := AuditEntry{
				InvocationID: invID,
				ToolCallID:   callID,
				TS:           time.Now(),
				Kind:         "bash",
				Tool:         "bash",
				Args:         truncate(cmd, 500),
				ExitCode:     auditCode,
			}
			if auditErr != nil {
				entry.Error = auditErr.Error()
			}
			auditFn(entry)
		}
	})
}

func (j *Job) preparePendingPostExitCleanupLocked() processGroupProof {
	j.cleanupPending = true
	if !j.groupProof.VerifiedAt.IsZero() && j.groupProof.ExitObservedAt.IsZero() {
		j.groupProof.ExitObservedAt = time.Now()
	}
	return j.snapshotGroupProofLocked()
}

func (j *Job) mergeLearnedGroupMembers(proof processGroupProof) {
	if len(proof.KnownMembers) == 0 {
		return
	}
	j.mu.Lock()
	j.groupProof.KnownMembers = mergeGroupMembers(j.groupProof.KnownMembers, proof.KnownMembers)
	stale := detachStaleVerifiedFDs(&j.groupProof)
	j.mu.Unlock()
	closeVerifiedGroupProof(&stale)
}

func hasIndependentGroupProof(proof processGroupProof) bool {
	if proof.JobToken != "" || proof.jobHandle != 0 || !proof.VerifiedAt.IsZero() {
		return true
	}
	for pid := range proof.KnownMembers {
		if pid != proof.Leader.PID {
			return true
		}
	}
	return false
}

func (j *Job) prepareAdoptedDowntimeTimeoutSummary(pid, pgid int, deadline time.Time, outFile, cwd string) (string, *BashOutputPreview) {
	timeoutNote := fmt.Sprintf("adopted job PID %d (pgid %d) exceeded deadline (%s) while harness was down; stopped verified process group; %s",
		pid, pgid, deadline.UTC().Format(time.RFC3339), pipeCaptureEndedNote(outFile))
	j.mu.Lock()
	if !j.finished && !j.unknown {
		j.RecoveryNote = timeoutNote
	}
	j.mu.Unlock()
	summary, preview := formatAdoptedSummaryPreview(outFile, cwd, fmt.Sprintf("(adopted background job stopped on adoption; %s)", timeoutNote), timeoutNote)
	return summary, preview
}

func (j *Job) stopAdoptedProcess(cause error, isTimeout bool) error {
	j.proofFDMu.Lock()
	j.mu.Lock()
	if j.finished || j.unknown {
		j.mu.Unlock()
		j.proofFDMu.Unlock()
		return nil
	}
	id := ProcessIdentityInfo{PID: j.PID, PGID: j.PGID, StartToken: j.ProcessStarted}
	outFile, cwd, monitorFailed := j.OutputFile, j.Cwd, j.monitorFailed
	auditFn := j.audit
	started, deadline := j.Started, j.Deadline
	monitorDone := false
	if j.monitorDone != nil {
		select {
		case <-j.monitorDone:
			monitorDone = true
		default:
		}
	}
	mustFinalize := monitorFailed || monitorDone
	pendingCleanup := j.cleanupPending || !j.groupProof.ExitObservedAt.IsZero()
	var proof processGroupProof
	if pendingCleanup {
		proof = j.preparePendingPostExitCleanupLocked()
	}
	j.mu.Unlock()

	var err error
	if pendingCleanup {
		err = os.ErrProcessDone
	} else {
		// Refresh verified PGID members while the leader may still be alive so any
		// reparented children remain verifiable if the leader exits or wait fails.
		proof = j.refreshVerifiedGroupMembersLocked()
		err = stopVerifiedProcessGroupFn(proof)
		if errors.Is(err, ErrProcessIdentityMismatch) && hasIndependentGroupProof(proof) {
			err = os.ErrProcessDone
		}
	}
	if errors.Is(err, ErrProcessIdentityMismatch) {
		j.proofFDMu.Unlock()
		summary, preview, _, _ := summarizeOutputFile(outFile, cwd)
		j.markUnknown(fmt.Sprintf("refused to signal PID %d: %v", id.PID, err), summary, preview)
		return err
	}
	if errors.Is(err, os.ErrProcessDone) {
		j.mu.Lock()
		proof = j.preparePendingPostExitCleanupLocked()
		j.mu.Unlock()
		j.persist()
		gerr := j.runStopGroupAfterExit(&proof)
		j.mergeLearnedGroupMembers(proof)
		j.proofFDMu.Unlock()
		if errors.Is(gerr, ErrProcessIdentityMismatch) {
			summary, preview, _, _ := summarizeOutputFile(outFile, cwd)
			j.markUnknown(fmt.Sprintf("refused to signal PID %d: %v", id.PID, gerr), summary, preview)
			return gerr
		}
		if gerr != nil && !errors.Is(gerr, os.ErrProcessDone) {
			j.recordStopError(gerr)
			return gerr
		}
	} else if err != nil {
		if isProcessAccessError(err) && hasIndependentGroupProof(proof) {
			// Leader PID denied access (for instance, a protected process reused
			// the PID or leader query is restricted); attempt cleanup of
			// independently verified descendants while keeping leader state
			// unresolved until it can be verified.
			descProof := cloneGroupProof(proof)
			gerr := j.runStopGroupAfterExit(&descProof)
			j.mergeLearnedGroupMembers(descProof)
			j.proofFDMu.Unlock()
			if gerr != nil && !errors.Is(gerr, os.ErrProcessDone) && !errors.Is(gerr, ErrProcessIdentityMismatch) {
				j.recordStopError(gerr)
				return gerr
			}
			j.recordStopError(err)
			return err
		}
		j.proofFDMu.Unlock()
		j.recordStopError(err)
		return err
	} else if mustFinalize {
		j.proofFDMu.Unlock()
		waitForAdoptedProcessGone(id, 2*time.Second)
		j.proofFDMu.Lock()
		j.mu.Lock()
		stillActive := !j.finished && !j.unknown
		if stillActive {
			proof = j.preparePendingPostExitCleanupLocked()
		}
		j.mu.Unlock()
		if !stillActive {
			j.proofFDMu.Unlock()
			return nil
		}
		j.persist()
		gerr := j.runStopGroupAfterExit(&proof)
		j.mergeLearnedGroupMembers(proof)
		j.proofFDMu.Unlock()
		if errors.Is(gerr, ErrProcessIdentityMismatch) {
			summary, preview, _, _ := summarizeOutputFile(outFile, cwd)
			j.markUnknown(fmt.Sprintf("refused to signal PID %d: %v", id.PID, gerr), summary, preview)
			return gerr
		}
		if gerr != nil && !errors.Is(gerr, os.ErrProcessDone) {
			j.recordStopError(gerr)
			return gerr
		}
	} else {
		j.proofFDMu.Unlock()
	}
	if mustFinalize {
		note := pipeCaptureEndedNote(outFile)
		summary, preview := formatAdoptedSummaryPreview(outFile, cwd, fmt.Sprintf("(adopted background job stopped; %s)", note), note)
		resErr := cause
		var exitCode int
		var exitUnknown bool
		if isTimeout {
			resErr = &timeoutError{timeout: max(0, deadline.Sub(started))}
			exitCode = -1
			j.mu.Lock()
			expiredOnAdoption := j.expiredOnAdoption
			pid, pgid := j.PID, j.PGID
			j.mu.Unlock()
			if expiredOnAdoption {
				summary, preview = j.prepareAdoptedDowntimeTimeoutSummary(pid, pgid, deadline, outFile, cwd)
			}
		} else {
			exitCode = -1
			exitUnknown = true
		}
		j.finishAdoptedWithAudit(JobResult{
			Output:          summary,
			Err:             resErr,
			ExitCode:        exitCode,
			ExitCodeUnknown: exitUnknown,
			Preview:         preview,
		}, auditFn, -1, resErr)
		return nil
	}
	j.persist()
	return nil
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
	return e.registry().cancelAll
}

// PrepareSessionCancel advances the registry's abort generation and snapshots
// currently running jobs so callers holding a turn lock can release the lock
// before signalling jobs, without racing against jobs started by a queued turn.
func (e *Executor) PrepareSessionCancel() func() {
	return e.registry().prepareCancelAll()
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
			if job.Status() == JobRunning {
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

func (e *Executor) backgroundMaxLifetime() time.Duration {
	if e.BackgroundMaxLifetime < 0 {
		return 0
	}
	if e.BackgroundMaxLifetime > 0 {
		return e.BackgroundMaxLifetime
	}
	return DefaultBackgroundMaxLifetime
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
	// Prune inactive session registries that have no running or undelivered jobs.
	for sid, reg := range e.sessionJobs {
		if sid == id || reg == nil {
			continue
		}
		keep := false
		for _, job := range reg.List() {
			if job.Status() == JobRunning || !job.Delivered() {
				keep = true
				break
			}
		}
		if !keep {
			delete(e.sessionJobs, sid)
			delete(e.recoveredSessions, sid)
		}
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
	if e.recoveredSessions != nil && e.recoveredSessions[oldID] {
		e.recoveredSessions[newID] = true
		delete(e.recoveredSessions, oldID)
	}
	if e.sessionID == oldID {
		e.sessionID = newID
	}
}
