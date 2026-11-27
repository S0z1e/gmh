package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/outputreceipt"
)

// JobUnknown indicates a previously started job whose post-crash live process
// cannot be safely verified and adopted (for example, because it exited while
// the harness was down or its PID was reused by another process).
const JobUnknown JobStatus = "unknown"

// JobIdentityKey uniquely identifies a job incarnation even when display IDs
// are reused across sessions or tests.
type JobIdentityKey struct {
	ID      string
	Started int64
}

// MakeJobIdentityKey constructs a stable lookup key from a job ID and start time.
func MakeJobIdentityKey(id string, started time.Time) JobIdentityKey {
	return JobIdentityKey{ID: id, Started: started.UnixNano()}
}

// DeliveryStatus summarizes what job notices have already been persisted to the
// session transcript or checkpoint so reopen does not deliver duplicates.
type DeliveryStatus struct {
	DoneDelivered   bool
	UnknownNoticed  bool
	AdoptionNoticed bool
}

// PersistedGroupProof stores serializable process-group verification metadata
// across restarts so pending post-exit cleanup remains safely revalidatable.
type PersistedGroupProof struct {
	UID               uint32         `json:"uid,omitempty"`
	HasUID            bool           `json:"has_uid,omitempty"`
	SID               int            `json:"sid,omitempty"`
	Sess              uint64         `json:"sess,omitempty"`
	OutputFile        string         `json:"output_file,omitempty"`
	LeaderPipeTargets []string       `json:"leader_pipe_targets,omitempty"`
	KnownMembers      map[int]string `json:"known_members,omitempty"`
	VerifiedAt        time.Time      `json:"verified_at,omitempty"`
	ExitObservedAt    time.Time      `json:"exit_observed_at,omitempty"`
	CleanupPending    bool           `json:"cleanup_pending,omitempty"`
}

// PersistedJob stores durable background-job metadata across harness crashes.
type PersistedJob struct {
	ID              string               `json:"id"`
	Started         time.Time            `json:"started"`
	Deadline        time.Time            `json:"deadline,omitempty"`
	Unlimited       bool                 `json:"unlimited,omitempty"`
	InvocationID    string               `json:"invocation_id,omitempty"`
	ToolCallID      string               `json:"tool_call_id,omitempty"`
	Command         string               `json:"command"`
	Cwd             string               `json:"cwd,omitempty"`
	OutputFile      string               `json:"output_file,omitempty"`
	OutputID        string               `json:"output_id,omitempty"`
	OutputReceipt   string               `json:"output_receipt,omitempty"`
	PID             int                  `json:"pid,omitempty"`
	PGID            int                  `json:"pgid,omitempty"`
	ProcessStarted  string               `json:"process_started,omitempty"`
	JobToken        string               `json:"job_token,omitempty"`
	GroupProof      *PersistedGroupProof `json:"group_proof,omitempty"`
	Status          JobStatus            `json:"status"`
	ExitCode        *int                 `json:"exit_code,omitempty"`
	ExitCodeUnknown bool                 `json:"exit_code_unknown,omitempty"`
	Error           string               `json:"error,omitempty"`
	FinishedAt      time.Time            `json:"finished_at,omitempty"`
	Delivered       bool                 `json:"delivered,omitempty"`
	StopRequested   bool                 `json:"stop_requested,omitempty"`
	AdoptionNoticed bool                 `json:"adoption_noticed,omitempty"`
	UnknownNoticed  bool                 `json:"unknown_noticed,omitempty"`
	RecoveryNote    string               `json:"recovery_note,omitempty"`
}

// RecoveredJob describes one retained job inspected while reopening a session.
type RecoveredJob struct {
	ID         string    `json:"id"`
	Command    string    `json:"command"`
	Cwd        string    `json:"cwd,omitempty"`
	OutputFile string    `json:"output_file,omitempty"`
	Status     JobStatus `json:"status"`
	PID        int       `json:"pid,omitempty"`
	PGID       int       `json:"pgid,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Adopted    bool      `json:"adopted,omitempty"`
	Delivered  bool      `json:"delivered,omitempty"`
}

func randomHexToken(nbytes int) string {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func jobSequenceNumber(id string) int {
	if !strings.HasPrefix(id, "bg-") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, "bg-"))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func safeJobRecordName(id string, started time.Time) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	prefix := b.String()
	if prefix == "" {
		prefix = "job"
	}
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}
	return fmt.Sprintf("%s-%016x.json", prefix, uint64(started.UnixNano()))
}

func savePersistedJob(jobsDir string, rec PersistedJob) error {
	if jobsDir == "" || rec.ID == "" {
		return nil
	}
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(jobsDir)
	if err != nil {
		return err
	}
	defer root.Close()

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	finalName := safeJobRecordName(rec.ID, rec.Started)
	tmpName := ".tmp-" + randomHexToken(8) + ".json"
	f, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = root.Remove(tmpName)
		return errors.Join(writeErr, syncErr, closeErr)
	}
	if err := root.Rename(tmpName, finalName); err != nil {
		_ = root.Remove(tmpName)
		return err
	}
	return nil
}

func removePersistedJob(jobsDir, id string, started time.Time) {
	if jobsDir == "" || id == "" {
		return
	}
	root, err := os.OpenRoot(jobsDir)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.Remove(safeJobRecordName(id, started))
}

func loadPersistedJobs(jobsDir string) ([]PersistedJob, error) {
	if jobsDir == "" {
		return nil, nil
	}
	root, err := os.OpenRoot(jobsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer root.Close()

	dirFile, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dirFile.Close()

	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []PersistedJob
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("non-regular persisted job file: %q", name)
		}
		if info.Size() > 256<<10 {
			return nil, fmt.Errorf("oversized persisted job file: %q", name)
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var rec PersistedJob
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("decode persisted job %q: %w", name, err)
		}
		if rec.ID == "" {
			continue
		}
		out = append(out, rec)
	}
	sortPersistedJobs(out)
	return out, nil
}

func sortPersistedJobs(records []PersistedJob) {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].Started.Equal(records[j].Started) {
			return records[i].Started.Before(records[j].Started)
		}
		si, sj := jobSequenceNumber(records[i].ID), jobSequenceNumber(records[j].ID)
		if si != sj {
			return si < sj
		}
		return records[i].ID < records[j].ID
	})
}

func prunePersistedJobs(jobsDir string, records []PersistedJob) {
	if jobsDir == "" || len(records) <= recentJobsLimit*2 {
		return
	}
	root, err := os.OpenRoot(jobsDir)
	if err != nil {
		return
	}
	defer root.Close()

	cutoff := time.Now().Add(-staleUndeliveredJobAge)
	excess := len(records) - recentJobsLimit
	for _, rec := range records {
		if excess <= 0 {
			break
		}
		// Never prune running, unknown, or undelivered records: unknown jobs
		// retain non-replay/notice state across restarts.
		if rec.Status != JobDone || !rec.Delivered {
			continue
		}
		finishedAt := rec.FinishedAt
		if finishedAt.IsZero() {
			finishedAt = rec.Started
		}
		if finishedAt.After(cutoff) {
			continue
		}
		if err := root.Remove(safeJobRecordName(rec.ID, rec.Started)); err == nil {
			excess--
		}
	}
}

func validRecoveredOutputPath(outputFile, jobOutputID, execOutputID, execOutputDir string) bool {
	// Fail closed unless this executor owns an initialized OutputDir.
	if outputFile == "" || execOutputDir == "" {
		return false
	}
	clean := filepath.Clean(outputFile)
	if !filepath.IsAbs(clean) {
		return false
	}
	base := filepath.Base(clean)
	if !strings.HasPrefix(base, "bash-") || !strings.HasSuffix(base, ".log") {
		return false
	}
	if execOutputID != "" && jobOutputID != "" && jobOutputID != execOutputID {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(execOutputDir), clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.ContainsRune(rel, filepath.Separator) {
		return false
	}
	root, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return false
	}
	defer root.Close()
	info, err := root.Lstat(base)
	return err == nil && info.Mode().IsRegular()
}

func summarizeOutputFile(path, cwd string) (summary string, preview *BashOutputPreview, bytes int64, ok bool) {
	if path == "" {
		return "", nil, 0, false
	}
	clean := filepath.Clean(path)
	root, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return "", nil, 0, false
	}
	defer root.Close()

	base := filepath.Base(clean)
	info, err := root.Lstat(base)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, 0, false
	}
	f, err := root.Open(base)
	if err != nil {
		return "", nil, 0, false
	}
	defer f.Close()

	sink := newBashOutputSink()
	_, err = io.Copy(sink, io.LimitReader(f, OutputFileMaxBytes+1))
	_ = sink.Close()
	if err != nil {
		return "", nil, 0, false
	}
	lines, readBytes, capped, head, tail := sink.snapshot()
	pv := bashPreview(head, tail, lines <= len(head))
	return FormatBashOutput(lines, readBytes, capped, head, tail, clean, cwd), &pv, readBytes, true
}

func intPtrCopy(v *int) *int {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

// CheckpointJobToPersisted converts a serialized checkpoint job record to the
// canonical PersistedJob representation used by recovery merging.
func CheckpointJobToPersisted(cj checkpoint.Job) PersistedJob {
	status := JobRunning
	switch cj.Status {
	case checkpoint.Done:
		status = JobDone
	case checkpoint.Unknown:
		status = JobUnknown
	}
	return PersistedJob{
		ID:              cj.ID,
		Started:         cj.Started,
		Deadline:        cj.Deadline,
		Unlimited:       cj.Unlimited,
		InvocationID:    cj.InvocationID,
		Command:         cj.Command,
		Cwd:             cj.Cwd,
		OutputFile:      cj.OutputFile,
		OutputID:        cj.OutputID,
		OutputReceipt:   cj.OutputReceipt,
		PID:             cj.PID,
		PGID:            cj.PGID,
		ProcessStarted:  cj.ProcessStarted,
		Status:          status,
		ExitCode:        intPtrCopy(cj.ExitCode),
		ExitCodeUnknown: cj.Status == checkpoint.Done && cj.ExitCode == nil,
		Error:           cj.Error,
		Delivered:       cj.Delivered,
		AdoptionNoticed: cj.AdoptionNoticed,
		UnknownNoticed:  cj.UnknownNoticed,
	}
}

func jobStatusRank(s JobStatus) int {
	switch s {
	case JobDone:
		return 3
	case JobUnknown:
		return 2
	case JobRunning:
		return 1
	default:
		return 0
	}
}

// MergeRecoveredRecord folds src into dst without discarding known identity,
// delivery, or higher-precedence terminal status.
func MergeRecoveredRecord(dst *PersistedJob, src PersistedJob) {
	if dst.ID == "" {
		*dst = src
		dst.ExitCode = intPtrCopy(src.ExitCode)
		return
	}
	if dst.Started.IsZero() && !src.Started.IsZero() {
		dst.Started = src.Started
	}
	if src.Unlimited {
		dst.Unlimited = true
		dst.Deadline = time.Time{}
	} else if !dst.Unlimited && dst.Deadline.IsZero() && !src.Deadline.IsZero() {
		dst.Deadline = src.Deadline
	}
	if dst.InvocationID == "" {
		dst.InvocationID = src.InvocationID
	}
	if dst.ToolCallID == "" {
		dst.ToolCallID = src.ToolCallID
	}
	if dst.Command == "" {
		dst.Command = src.Command
	}
	if dst.Cwd == "" {
		dst.Cwd = src.Cwd
	}
	if dst.OutputFile == "" {
		dst.OutputFile = src.OutputFile
	}
	if dst.OutputID == "" {
		dst.OutputID = src.OutputID
	}
	if dst.OutputReceipt == "" {
		dst.OutputReceipt = src.OutputReceipt
	}
	if dst.PID == 0 && src.PID > 0 {
		dst.PID = src.PID
	}
	if dst.PGID == 0 && src.PGID > 0 {
		dst.PGID = src.PGID
	}
	if dst.ProcessStarted == "" {
		dst.ProcessStarted = src.ProcessStarted
	}
	if jobStatusRank(src.Status) > jobStatusRank(dst.Status) {
		dst.Status = src.Status
		if src.ExitCode != nil {
			dst.ExitCode = intPtrCopy(src.ExitCode)
		}
		if src.ExitCodeUnknown {
			dst.ExitCodeUnknown = true
		}
		if src.Error != "" {
			dst.Error = src.Error
		}
		if src.RecoveryNote != "" {
			dst.RecoveryNote = src.RecoveryNote
		}
		if !src.FinishedAt.IsZero() {
			dst.FinishedAt = src.FinishedAt
		}
	} else if dst.Status == src.Status {
		if dst.ExitCode == nil && src.ExitCode != nil {
			dst.ExitCode = intPtrCopy(src.ExitCode)
		}
		if !dst.ExitCodeUnknown && src.ExitCodeUnknown {
			dst.ExitCodeUnknown = true
		}
		if dst.Error == "" && src.Error != "" {
			dst.Error = src.Error
		}
		if dst.RecoveryNote == "" && src.RecoveryNote != "" {
			dst.RecoveryNote = src.RecoveryNote
		}
		if dst.FinishedAt.IsZero() && !src.FinishedAt.IsZero() {
			dst.FinishedAt = src.FinishedAt
		}
	}
	if src.Delivered {
		dst.Delivered = true
	}
	if src.StopRequested {
		dst.StopRequested = true
	}
	if src.AdoptionNoticed {
		dst.AdoptionNoticed = true
	}
	if src.UnknownNoticed {
		dst.UnknownNoticed = true
	}
}

func collectMergedRecoveryRecords(diskRecords []PersistedJob, cpJobs []checkpoint.Job, transcriptHints []PersistedJob) []PersistedJob {
	merged := map[JobIdentityKey]*PersistedJob{}
	order := make([]JobIdentityKey, 0, len(diskRecords)+len(cpJobs)+len(transcriptHints))
	byID := map[string]JobIdentityKey{}
	addSource := func(src PersistedJob) {
		if src.ID == "" {
			return
		}
		if src.Status == "" {
			src.Status = JobRunning
		}
		key := MakeJobIdentityKey(src.ID, src.Started)
		if existing, ok := merged[key]; ok {
			MergeRecoveredRecord(existing, src)
			return
		}
		if src.Started.IsZero() {
			if prevKey, ok := byID[src.ID]; ok {
				MergeRecoveredRecord(merged[prevKey], src)
				return
			}
		}
		cp := src
		cp.ExitCode = intPtrCopy(src.ExitCode)
		merged[key] = &cp
		order = append(order, key)
		byID[src.ID] = key
	}

	for _, rec := range diskRecords {
		addSource(rec)
	}
	for _, cj := range cpJobs {
		addSource(CheckpointJobToPersisted(cj))
	}
	for _, th := range transcriptHints {
		addSource(th)
	}

	out := make([]PersistedJob, 0, len(order))
	for _, key := range order {
		out = append(out, *merged[key])
	}
	sortPersistedJobs(out)
	return out
}

func pipeCaptureEndedNote(outputFile string) string {
	if outputFile != "" {
		return fmt.Sprintf("live stdout/stderr pipe closed when the previous harness exited; pre-crash captured output remains in %s, while post-crash writes cannot be captured", outputFile)
	}
	return "live stdout/stderr pipe closed when the previous harness exited; post-crash output cannot be captured"
}

type adoptionMonitor struct {
	job         *Job
	ctx         context.Context
	cancelCause context.CancelCauseFunc
	closeCh     chan struct{}
}

func groupProofFromPersisted(rec PersistedJob, id ProcessIdentityInfo, outFile string) processGroupProof {
	if rec.GroupProof == nil {
		return processGroupProof{Leader: id, JobToken: rec.JobToken, OutputFile: outFile}
	}
	return processGroupProof{
		Leader:            id,
		UID:               rec.GroupProof.UID,
		HasUID:            rec.GroupProof.HasUID,
		SID:               rec.GroupProof.SID,
		Sess:              uintptr(rec.GroupProof.Sess),
		JobToken:          rec.JobToken,
		OutputFile:        outFile,
		LeaderPipeTargets: append([]string(nil), rec.GroupProof.LeaderPipeTargets...),
		KnownMembers:      mergeGroupMembers(nil, rec.GroupProof.KnownMembers),
		VerifiedAt:        rec.GroupProof.VerifiedAt,
		ExitObservedAt:    rec.GroupProof.ExitObservedAt,
	}
}

func persistedGroupProofFromProof(proof processGroupProof, cleanupPending bool) *PersistedGroupProof {
	var knownCopy map[int]string
	if len(proof.KnownMembers) > 0 {
		knownCopy = make(map[int]string, len(proof.KnownMembers))
		for k, v := range proof.KnownMembers {
			knownCopy[k] = v
		}
	}
	return &PersistedGroupProof{
		UID:               proof.UID,
		HasUID:            proof.HasUID,
		SID:               proof.SID,
		Sess:              uint64(proof.Sess),
		OutputFile:        proof.OutputFile,
		LeaderPipeTargets: append([]string(nil), proof.LeaderPipeTargets...),
		KnownMembers:      knownCopy,
		VerifiedAt:        proof.VerifiedAt,
		ExitObservedAt:    proof.ExitObservedAt,
		CleanupPending:    cleanupPending,
	}
}

func effectiveRecoveredDeadline(rec PersistedJob) (time.Time, bool) {
	if rec.Unlimited {
		return time.Time{}, true
	}
	if !rec.Deadline.IsZero() {
		return rec.Deadline, false
	}
	return rec.Started.Add(DefaultBackgroundMaxLifetime), false
}

func waitForAdoptedProcessGone(id ProcessIdentityInfo, maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		info := inspectProcess(id.PID)
		if !info.Alive || info.Zombie || !id.Matches(info) {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func (e *Executor) newRecoveredJobObject(rec PersistedJob, jobsDir string, status JobStatus, delivered, adoptionNoticed bool, note string, res JobResult) *Job {
	doneCh := make(chan struct{})
	if status != JobRunning {
		close(doneCh)
	}
	finishedAt := time.Time{}
	if status != JobRunning {
		finishedAt = rec.FinishedAt
		if finishedAt.IsZero() {
			finishedAt = rec.Started
		}
	}
	return &Job{
		InvocationID:    rec.InvocationID,
		ToolCallID:      rec.ToolCallID,
		ID:              rec.ID,
		Command:         rec.Command,
		Cwd:             rec.Cwd,
		OutputFile:      rec.OutputFile,
		OutputID:        rec.OutputID,
		OutputReceipt:   rec.OutputReceipt,
		PID:             rec.PID,
		PGID:            rec.PGID,
		ProcessStarted:  rec.ProcessStarted,
		JobToken:        rec.JobToken,
		Adopted:         status == JobRunning,
		RecoveryNote:    note,
		jobsDir:         jobsDir,
		outputDir:       e.OutputDir,
		exec:            e,
		audit:           e.Audit,
		retained:        true,
		Started:         rec.Started,
		Deadline:        rec.Deadline,
		Unlimited:       rec.Unlimited,
		done:            doneCh,
		finished:        status == JobDone,
		unknown:         status == JobUnknown,
		finishedAt:      finishedAt,
		delivered:       delivered,
		stopRequested:   rec.StopRequested,
		adoptionNoticed: adoptionNoticed,
		result:          res,
	}
}

func summaryFromRecoveredJob(job *Job) RecoveredJob {
	snap := job.Snapshot()
	var exitCode *int
	reason := snap.RecoveryNote
	if snap.Status == JobDone {
		if !snap.Result.ExitCodeUnknown {
			code := snap.Result.ExitCode
			exitCode = &code
		}
		if reason == "" && snap.Result.Err != nil {
			reason = snap.Result.Err.Error()
		}
	}
	return RecoveredJob{
		ID:         snap.ID,
		Command:    snap.Command,
		Cwd:        snap.Cwd,
		OutputFile: snap.OutputFile,
		Status:     snap.Status,
		PID:        snap.Identity.PID,
		PGID:       snap.Identity.PGID,
		ExitCode:   exitCode,
		Reason:     reason,
		Adopted:    snap.Adopted,
		Delivered:  snap.Delivered,
	}
}

func (e *Executor) resolveRecoveredJob(rec PersistedJob, d DeliveryStatus, jobsDir string) (*Job, *adoptionMonitor) {
	if !validRecoveredOutputPath(rec.OutputFile, rec.OutputID, e.OutputID, e.OutputDir) {
		rec.OutputFile = ""
	}
	receiptExists := rec.OutputFile != "" && e.OutputDir != "" && outputreceipt.Exists(e.OutputDir, rec.OutputFile)
	doneDelivered := rec.Delivered || d.DoneDelivered || receiptExists
	unknownNoticed := rec.UnknownNoticed || rec.Delivered || d.UnknownNoticed
	adoptionNoticed := rec.AdoptionNoticed || d.AdoptionNoticed
	if rec.Started.IsZero() {
		rec.Started = time.Now()
	}
	deadline, unlimited := effectiveRecoveredDeadline(rec)
	rec.Deadline = deadline
	rec.Unlimited = unlimited

	switch rec.Status {
	case JobDone:
		rec.OutputReceipt = ""
		if doneDelivered && rec.OutputFile != "" && e.OutputDir != "" {
			if receiptExists || rec.OutputReceipt != "" || outputreceipt.Path(e.OutputDir, rec.OutputFile) != "" {
				rec.OutputReceipt = filepath.Base(rec.OutputFile)
			}
		}
		summary, preview, _, hasFile := summarizeOutputFile(rec.OutputFile, rec.Cwd)
		if !hasFile && rec.Error == "" {
			summary = "(output unavailable after restart)"
		}
		var errVal error
		if rec.Error != "" {
			errVal = errors.New(rec.Error)
		}
		exitUnknown := rec.ExitCodeUnknown || rec.ExitCode == nil
		code := -1
		if rec.ExitCode != nil {
			code = *rec.ExitCode
			exitUnknown = false
		}
		job := e.newRecoveredJobObject(rec, jobsDir, JobDone, doneDelivered, adoptionNoticed, rec.RecoveryNote, JobResult{
			Output:          summary,
			Err:             errVal,
			ExitCode:        code,
			ExitCodeUnknown: exitUnknown,
			Preview:         preview,
		})
		if doneDelivered != rec.Delivered && jobsDir != "" {
			_ = savePersistedJob(jobsDir, job.persistedSnapshot())
		}
		return job, nil

	case JobUnknown:
		rec.OutputReceipt = ""
		reason := rec.RecoveryNote
		if reason == "" {
			reason = rec.Error
		}
		if reason == "" {
			reason = "process state could not be verified after harness restart"
		}
		summary, preview, _, _ := summarizeOutputFile(rec.OutputFile, rec.Cwd)
		job := e.newRecoveredJobObject(rec, jobsDir, JobUnknown, unknownNoticed, adoptionNoticed, reason, JobResult{
			Output:          summary,
			Err:             errors.New(reason),
			ExitCode:        -1,
			ExitCodeUnknown: true,
			Preview:         preview,
		})
		if jobsDir != "" && (unknownNoticed != rec.UnknownNoticed || rec.RecoveryNote != reason) {
			_ = savePersistedJob(jobsDir, job.persistedSnapshot())
		}
		return job, nil

	default: // JobRunning: validate live process identity and group before adopting.
		rec.OutputReceipt = ""
		id := ProcessIdentityInfo{PID: rec.PID, PGID: rec.PGID, StartToken: rec.ProcessStarted}
		expiredDuringDowntime := !rec.Unlimited && !rec.Deadline.IsZero() && !time.Now().Before(rec.Deadline)
		info, inspectErr := inspectProcessErr(rec.PID)
		if inspectErr != nil && id.Valid() {
			proof := groupProofFromPersisted(rec, id, rec.OutputFile)
			var note string
			if expiredDuringDowntime {
				note = fmt.Sprintf("adopted process PID %d (pgid %d); deadline (%s) passed while harness was down, process inspection error (%v) — job remains stoppable; %s",
					id.PID, id.PGID, rec.Deadline.UTC().Format(time.RFC3339), inspectErr, pipeCaptureEndedNote(rec.OutputFile))
			} else {
				note = fmt.Sprintf("adopted process PID %d (pgid %d); process inspection error (%v) — job remains stoppable; %s",
					id.PID, id.PGID, inspectErr, pipeCaptureEndedNote(rec.OutputFile))
			}
			job := e.newRecoveredJobObject(rec, jobsDir, JobRunning, false, adoptionNoticed, note, JobResult{})
			job.WaitError = inspectErr.Error()
			job.expiredOnAdoption = expiredDuringDowntime
			job.groupProof = proof
			if !proof.ExitObservedAt.IsZero() {
				job.cleanupPending = true
			}
			return e.attachAdoptedStopControls(job, jobsDir)
		}
		if inspectErr == nil && id.Matches(info) {
			proof := groupProofFromPersisted(rec, id, rec.OutputFile)
			populateGroupProofIdentity(&proof, info)
			captureVerifiedGroupProof(&proof)
			extraNote := ""
			if !rec.Unlimited && !rec.Deadline.IsZero() && !time.Now().Before(rec.Deadline) {
				stopErr := stopVerifiedProcessGroupWithProof(proof)
				switch {
				case errors.Is(stopErr, ErrProcessIdentityMismatch):
					closeVerifiedGroupProof(&proof)
					reason := fmt.Sprintf("refused to attach to PID %d: process identity, group, or start token mismatch (PID reused)", id.PID)
					if rec.FinishedAt.IsZero() {
						rec.FinishedAt = time.Now()
					}
					summary, preview, _, _ := summarizeOutputFile(rec.OutputFile, rec.Cwd)
					job := e.newRecoveredJobObject(rec, jobsDir, JobUnknown, unknownNoticed, adoptionNoticed, reason, JobResult{
						Output:          summary,
						Err:             errors.New(reason),
						ExitCode:        -1,
						ExitCodeUnknown: true,
						Preview:         preview,
					})
					if jobsDir != "" {
						_ = savePersistedJob(jobsDir, job.persistedSnapshot())
					}
					return job, nil
				case stopErr == nil || errors.Is(stopErr, os.ErrProcessDone):
					waitForAdoptedProcessGone(id, 2*time.Second)
					if !proof.VerifiedAt.IsZero() && proof.ExitObservedAt.IsZero() {
						proof.ExitObservedAt = time.Now()
					}
					groupProof := cloneGroupProof(proof)
					groupProof.verifiedFDs = proof.verifiedFDs
					if jobsDir != "" {
						pendingRec := rec
						pendingRec.GroupProof = persistedGroupProofFromProof(groupProof, true)
						_ = savePersistedJob(jobsDir, pendingRec)
					}
					stopGroupAfterExitTrackFn := stopAdoptedProcessGroupAfterExitTrack
					if e.stopAdoptedGroupAfterExit != nil {
						stopGroupAfterExitTrackFn = func(p *processGroupProof) error {
							return e.stopAdoptedGroupAfterExit(*p)
						}
					}
					if gerr := stopGroupAfterExitTrackFn(&groupProof); errors.Is(gerr, ErrProcessIdentityMismatch) {
						closeVerifiedGroupProof(&proof)
						reason := fmt.Sprintf("refused to attach to PID %d: %v", id.PID, gerr)
						if rec.FinishedAt.IsZero() {
							rec.FinishedAt = time.Now()
						}
						summary, preview, _, _ := summarizeOutputFile(rec.OutputFile, rec.Cwd)
						job := e.newRecoveredJobObject(rec, jobsDir, JobUnknown, unknownNoticed, adoptionNoticed, reason, JobResult{
							Output:          summary,
							Err:             errors.New(reason),
							ExitCode:        -1,
							ExitCodeUnknown: true,
							Preview:         preview,
						})
						if jobsDir != "" {
							_ = savePersistedJob(jobsDir, job.persistedSnapshot())
						}
						return job, nil
					} else if gerr != nil && !errors.Is(gerr, os.ErrProcessDone) {
						proof.KnownMembers = mergeGroupMembers(proof.KnownMembers, groupProof.KnownMembers)
						extraNote = fmt.Sprintf("deadline (%s) passed while harness was down, group stop failed (%v); ", rec.Deadline.UTC().Format(time.RFC3339), gerr)
						break
					}
					closeVerifiedGroupProof(&proof)
					timeoutDur := max(0, rec.Deadline.Sub(rec.Started))
					timeoutErr := &timeoutError{timeout: timeoutDur}
					note := fmt.Sprintf("adopted job PID %d (pgid %d) exceeded deadline (%s) while harness was down; stopped verified process group; %s",
						id.PID, id.PGID, rec.Deadline.UTC().Format(time.RFC3339), pipeCaptureEndedNote(rec.OutputFile))
					summary, preview := formatAdoptedSummaryPreview(rec.OutputFile, rec.Cwd, fmt.Sprintf("(adopted background job stopped on adoption; %s)", note), note)
					job := e.newRecoveredJobObject(rec, jobsDir, JobRunning, false, adoptionNoticed, note, JobResult{})
					job.Adopted = true
					job.finishAdoptedWithAudit(JobResult{
						Output:   summary,
						Err:      timeoutErr,
						ExitCode: -1,
						Preview:  preview,
					}, e.Audit, -1, timeoutErr)
					return job, nil
				default:
					extraNote = fmt.Sprintf("deadline (%s) passed while harness was down, stop failed (%v); ", rec.Deadline.UTC().Format(time.RFC3339), stopErr)
				}
			}
			note := fmt.Sprintf("adopted live OS process PID %d (pgid %d); %s%s", id.PID, id.PGID, extraNote, pipeCaptureEndedNote(rec.OutputFile))
			job := e.newRecoveredJobObject(rec, jobsDir, JobRunning, false, adoptionNoticed, note, JobResult{})
			job.expiredOnAdoption = expiredDuringDowntime
			job.groupProof = cloneGroupProof(proof)
			job.groupProof.verifiedFDs = proof.verifiedFDs
			if !proof.ExitObservedAt.IsZero() {
				job.cleanupPending = true
			}
			return e.attachAdoptedStopControls(job, jobsDir)
		}

		if (!info.Alive || info.Zombie) && id.Valid() && rec.GroupProof != nil && !rec.GroupProof.VerifiedAt.IsZero() {
			proof := groupProofFromPersisted(rec, id, rec.OutputFile)
			liveMember := rec.GroupProof.CleanupPending
			var scanErr error
			if !liveMember {
				liveMember, scanErr = hasVerifiedLiveGroupMember(proof)
			}
			if errors.Is(scanErr, ErrProcessIdentityMismatch) {
				liveMember = false
				scanErr = nil
			}
			if liveMember || scanErr != nil {
				if proof.ExitObservedAt.IsZero() {
					proof.ExitObservedAt = time.Now()
				}
				if jobsDir != "" {
					pendingRec := rec
					pendingRec.GroupProof = persistedGroupProofFromProof(proof, true)
					_ = savePersistedJob(jobsDir, pendingRec)
				}
				stopGroupAfterExitTrackFn := stopAdoptedProcessGroupAfterExitTrack
				if e.stopAdoptedGroupAfterExit != nil {
					stopGroupAfterExitTrackFn = func(p *processGroupProof) error {
						return e.stopAdoptedGroupAfterExit(*p)
					}
				}
				gerr := stopGroupAfterExitTrackFn(&proof)
				if errors.Is(gerr, ErrProcessIdentityMismatch) {
					reason := fmt.Sprintf("refused to signal PID %d: %v", id.PID, gerr)
					if rec.FinishedAt.IsZero() {
						rec.FinishedAt = time.Now()
					}
					summary, preview, _, _ := summarizeOutputFile(rec.OutputFile, rec.Cwd)
					job := e.newRecoveredJobObject(rec, jobsDir, JobUnknown, unknownNoticed, adoptionNoticed, reason, JobResult{
						Output:          summary,
						Err:             errors.New(reason),
						ExitCode:        -1,
						ExitCodeUnknown: true,
						Preview:         preview,
					})
					if jobsDir != "" {
						_ = savePersistedJob(jobsDir, job.persistedSnapshot())
					}
					return job, nil
				}
				if gerr == nil || errors.Is(gerr, os.ErrProcessDone) {
					resErr := error(errAdoptedProcessExited)
					isTimedOut := !rec.Unlimited && !rec.Deadline.IsZero() && !time.Now().Before(rec.Deadline)
					if isTimedOut {
						resErr = &timeoutError{timeout: max(0, rec.Deadline.Sub(rec.Started))}
					}
					note := fmt.Sprintf("completed pending post-exit group cleanup for PID %d (pgid %d) after harness restart; %s",
						id.PID, id.PGID, pipeCaptureEndedNote(rec.OutputFile))
					summary, preview := formatAdoptedSummaryPreview(rec.OutputFile, rec.Cwd, fmt.Sprintf("(adopted background job exited; %s)", note), note)
					job := e.newRecoveredJobObject(rec, jobsDir, JobRunning, false, adoptionNoticed, note, JobResult{})
					job.Adopted = true
					job.finishAdoptedWithAudit(JobResult{
						Output:          summary,
						Err:             resErr,
						ExitCode:        -1,
						ExitCodeUnknown: !isTimedOut,
						Preview:         preview,
					}, e.Audit, -1, resErr)
					return job, nil
				}
				note := fmt.Sprintf("adopted pending group cleanup for exited PID %d (pgid %d); stop error (%v) — job remains stoppable; %s",
					id.PID, id.PGID, gerr, pipeCaptureEndedNote(rec.OutputFile))
				job := e.newRecoveredJobObject(rec, jobsDir, JobRunning, false, adoptionNoticed, note, JobResult{})
				job.WaitError = gerr.Error()
				job.monitorFailed = true
				job.cleanupPending = true
				job.groupProof = cloneGroupProof(proof)
				return e.attachAdoptedStopControls(job, jobsDir)
			}
		}

		var reason string
		switch {
		case !id.Valid():
			reason = "process identity, group, or start token missing after harness restart; refusing blind reattach"
		case info.Alive && !info.Zombie:
			reason = fmt.Sprintf("refused to attach to PID %d: process identity, group, or start token mismatch (PID reused)", id.PID)
		default:
			reason = fmt.Sprintf("previous process PID %d is no longer running (exited while harness was down; exit status unavailable)", id.PID)
		}
		if rec.FinishedAt.IsZero() {
			rec.FinishedAt = time.Now()
		}
		summary, preview, _, _ := summarizeOutputFile(rec.OutputFile, rec.Cwd)
		job := e.newRecoveredJobObject(rec, jobsDir, JobUnknown, unknownNoticed, adoptionNoticed, reason, JobResult{
			Output:          summary,
			Err:             errors.New(reason),
			ExitCode:        -1,
			ExitCodeUnknown: true,
			Preview:         preview,
		})
		if jobsDir != "" {
			_ = savePersistedJob(jobsDir, job.persistedSnapshot())
		}
		return job, nil
	}
}

func (e *Executor) attachAdoptedStopControls(job *Job, jobsDir string) (*Job, *adoptionMonitor) {
	waitCtx, cancelCause := context.WithCancelCause(context.Background())
	closeCh := make(chan struct{})
	var closeOnce sync.Once
	markClosed := func() {
		closeOnce.Do(func() { close(closeCh) })
	}
	job.monitorDone = make(chan struct{})
	job.cancel = func() { cancelCause(context.Canceled) }
	job.closeStop = func() error {
		markClosed()
		stopErr := job.stopAdoptedProcess(ErrExecutorClosed, false)
		if errors.Is(stopErr, ErrProcessIdentityMismatch) {
			cancelCause(ErrProcessIdentityMismatch)
			return stopErr
		}
		if stopErr != nil {
			return stopErr
		}
		cancelCause(ErrExecutorClosed)
		return nil
	}
	job.closeCancel = func() { _ = job.closeStop() }
	if job.OutputFile != "" && e.OutputDir != "" {
		e.markActiveOutput(e.OutputDir, job.OutputFile)
	}
	if jobsDir != "" {
		_ = savePersistedJob(jobsDir, job.persistedSnapshot())
	}
	return job, &adoptionMonitor{job: job, ctx: waitCtx, cancelCause: cancelCause, closeCh: closeCh}
}

// JobsDirPath returns the directory storing durable background-job metadata.
func (e *Executor) JobsDirPath() string {
	if e.JobsDir != "" {
		return e.JobsDir
	}
	if e.OutputDir != "" {
		return filepath.Clean(e.OutputDir) + "-jobs"
	}
	return ""
}

// SessionRecovered reports whether recovery has already run for sessionID in
// this Executor lifecycle.
func (e *Executor) SessionRecovered(sessionID string) bool {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	return e.recoveredSessions[sessionID]
}

// RecoverSessionJobs reloads retained jobs from durable job records, the latest
// checkpoint, and the repaired transcript, validates live OS process identity,
// and adopts verified running processes into the session registry.
func (e *Executor) RecoverSessionJobs(
	sessionID string,
	cpJobs []checkpoint.Job,
	transcriptHints []PersistedJob,
	deliveries map[JobIdentityKey]DeliveryStatus,
) ([]RecoveredJob, error) {
	e.jobsMu.Lock()
	for {
		if e.jobsClosed {
			e.jobsMu.Unlock()
			return nil, ErrExecutorClosed
		}
		if e.recoveredSessions != nil && e.recoveredSessions[sessionID] {
			reg := e.registryForRecoveryLocked(sessionID)
			e.jobsMu.Unlock()
			return recoveredSummaryFromJobs(reg), nil
		}
		if waitCh := e.recovering[sessionID]; waitCh != nil {
			e.jobsMu.Unlock()
			<-waitCh
			e.jobsMu.Lock()
			continue
		}
		if e.recoveredSessions == nil {
			e.recoveredSessions = map[string]bool{}
		}
		if e.recovering == nil {
			e.recovering = map[string]chan struct{}{}
		}
		gate := make(chan struct{})
		e.recovering[sessionID] = gate
		e.jobsMu.Unlock()
		defer func() {
			e.jobsMu.Lock()
			delete(e.recovering, sessionID)
			close(gate)
			e.jobsMu.Unlock()
		}()
		break
	}

	jobsDir := e.JobsDirPath()
	diskRecords, loadErr := loadPersistedJobs(jobsDir)
	if loadErr != nil {
		return nil, loadErr
	}

	records := collectMergedRecoveryRecords(diskRecords, cpJobs, transcriptHints)

	// A job already in this session's registry is the live incarnation. Recovery
	// must not build a second copy and rewrite its record: that races the live
	// job's own persist, including after the turn that owns the temp directory
	// has returned.
	e.jobsMu.Lock()
	live := map[JobIdentityKey]bool{}
	if reg := e.registryForRecoveryLocked(sessionID); reg != nil {
		for _, j := range reg.List() {
			live[MakeJobIdentityKey(j.ID, j.Started)] = true
		}
	}
	e.jobsMu.Unlock()

	var recoveredJobs []*Job
	var summaries []RecoveredJob
	var monitors []adoptionMonitor
	maxSeq := 0
	for _, rec := range records {
		if seq := jobSequenceNumber(rec.ID); seq > maxSeq {
			maxSeq = seq
		}
		key := MakeJobIdentityKey(rec.ID, rec.Started)
		if live[key] {
			continue
		}
		job, mon := e.resolveRecoveredJob(rec, deliveries[key], jobsDir)
		recoveredJobs = append(recoveredJobs, job)
		summaries = append(summaries, summaryFromRecoveredJob(job))
		if mon != nil {
			monitors = append(monitors, *mon)
		}
	}

	e.jobsMu.Lock()
	if e.jobsClosed {
		e.jobsMu.Unlock()
		for _, m := range monitors {
			m.job.Cancel()
			m.job.closeProofFDs()
			if m.job.OutputFile != "" && e.OutputDir != "" {
				e.unmarkActiveOutput(e.OutputDir, m.job.OutputFile)
			}
		}
		return nil, ErrExecutorClosed
	}
	if e.sessionJobs == nil {
		e.sessionJobs = map[string]*JobRegistry{}
	}
	useCurrent := sessionID == e.sessionID || (e.sessionID == "" && e.sessionJobs[sessionID] == nil)
	var reg *JobRegistry
	if useCurrent {
		reg = e.jobs
		if sessionID != "" && e.sessionID == "" {
			e.sessionID = sessionID
		}
	} else {
		reg = e.sessionJobs[sessionID]
	}
	if reg == nil {
		reg = &JobRegistry{}
	}
	existingKeys := map[JobIdentityKey]bool{}
	reg.mu.Lock()
	for _, j := range reg.listLocked() {
		existingKeys[MakeJobIdentityKey(j.ID, j.Started)] = true
		if seq := jobSequenceNumber(j.ID); seq > maxSeq {
			maxSeq = seq
		}
	}
	reg.mu.Unlock()
	for _, j := range recoveredJobs {
		if existingKeys[MakeJobIdentityKey(j.ID, j.Started)] {
			continue
		}
		reg.addPreserved(j)
	}
	reg.mu.Lock()
	if maxSeq > reg.next {
		reg.next = maxSeq
	}
	reg.mu.Unlock()
	if maxSeq > e.nextJobSeq {
		e.nextJobSeq = maxSeq
	}
	if useCurrent {
		e.jobs = reg
	}
	e.sessionJobs[sessionID] = reg
	e.recoveredSessions[sessionID] = true
	e.jobsMu.Unlock()

	audit := e.Audit
	for _, m := range monitors {
		go e.monitorAdoptedJob(m, audit)
	}
	prunePersistedJobs(jobsDir, diskRecords)
	return summaries, nil
}

func (e *Executor) registryForRecoveryLocked(sessionID string) *JobRegistry {
	if sessionID == e.sessionID || (e.sessionID == "" && (e.sessionJobs == nil || e.sessionJobs[sessionID] == nil)) {
		return e.jobs
	}
	if e.sessionJobs != nil {
		return e.sessionJobs[sessionID]
	}
	return nil
}

func recoveredSummaryFromJobs(reg *JobRegistry) []RecoveredJob {
	if reg == nil {
		return nil
	}
	var out []RecoveredJob
	for _, job := range reg.List() {
		snap := job.Snapshot()
		if snap.Status == JobRunning && !snap.Adopted {
			continue
		}
		out = append(out, summaryFromRecoveredJob(job))
	}
	return out
}

func (e *Executor) monitorAdoptedJob(m adoptionMonitor, audit func(AuditEntry)) {
	job := m.job
	ctx := m.ctx
	cancelCause := m.cancelCause
	defer func() {
		if job.Status() != JobRunning && job.OutputFile != "" && job.outputDir != "" {
			e.unmarkActiveOutput(job.outputDir, job.OutputFile)
		}
		if job.monitorDone != nil {
			close(job.monitorDone)
		}
	}()
	snap := job.Snapshot()
	var timedOut bool
	var deadlineStopMu sync.Mutex
	var deadlineStopErr error
	if !snap.Unlimited && !snap.Deadline.IsZero() {
		var fallbackMu sync.Mutex
		var activeTimer *time.Timer
		var fallbackTimer *time.Timer
		var stopped bool
		var scheduleAttempt func(delay time.Duration, backoff time.Duration)
		scheduleAttempt = func(delay time.Duration, backoff time.Duration) {
			fallbackMu.Lock()
			if stopped {
				fallbackMu.Unlock()
				return
			}
			activeTimer = time.AfterFunc(delay, func() {
				if job.Status() != JobRunning || ctx.Err() != nil {
					return
				}
				deadlineStopMu.Lock()
				if job.Status() != JobRunning || ctx.Err() != nil {
					deadlineStopMu.Unlock()
					return
				}
				stopErr := job.stopAdoptedProcess(nil, true)
				if errors.Is(stopErr, ErrProcessIdentityMismatch) {
					deadlineStopMu.Unlock()
					if cancelCause != nil {
						cancelCause(ErrProcessIdentityMismatch)
					}
					return
				}
				if stopErr != nil {
					deadlineStopErr = stopErr
					deadlineStopMu.Unlock()
					next := min(backoff*2, 2*time.Second)
					scheduleAttempt(backoff, next)
					return
				}
				timedOut = true
				if cancelCause != nil {
					fallbackMu.Lock()
					if !stopped {
						fallbackTimer = time.AfterFunc(2*time.Second, func() {
							cancelCause(&timeoutError{timeout: max(0, snap.Deadline.Sub(snap.Started))})
						})
					}
					fallbackMu.Unlock()
				}
				deadlineStopMu.Unlock()
			})
			fallbackMu.Unlock()
		}
		remaining := max(0, time.Until(snap.Deadline))
		scheduleAttempt(remaining, 100*time.Millisecond)
		stopFn := func() {
			fallbackMu.Lock()
			stopped = true
			if activeTimer != nil {
				activeTimer.Stop()
			}
			if fallbackTimer != nil {
				fallbackTimer.Stop()
			}
			fallbackMu.Unlock()
		}
		job.mu.Lock()
		if job.finished || job.unknown {
			job.mu.Unlock()
			stopFn()
		} else {
			job.stopDeadline = stopFn
			job.mu.Unlock()
		}
	}

	job.mu.Lock()
	needsProofCapture := job.groupProof.VerifiedAt.IsZero()
	job.mu.Unlock()
	if needsProofCapture {
		job.proofFDMu.Lock()
		refreshed := job.refreshVerifiedGroupMembersLocked()
		job.proofFDMu.Unlock()
		if !refreshed.VerifiedAt.IsZero() {
			needsProofCapture = false
		}
	}
	var proofWg sync.WaitGroup
	var stopProofRefresh chan struct{}
	if needsProofCapture {
		stopProofRefresh = make(chan struct{})
		proofWg.Add(1)
		go func() {
			defer proofWg.Done()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopProofRefresh:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					job.proofFDMu.Lock()
					refreshed := job.refreshVerifiedGroupMembersLocked()
					job.proofFDMu.Unlock()
					if !refreshed.VerifiedAt.IsZero() {
						return
					}
				}
			}
		}()
	}

	waitFn := e.waitAdopted
	if waitFn == nil {
		waitFn = waitAdoptedProcess
	}
	exitCode, exitKnown, err := waitFn(ctx, snap.Identity)
	if stopProofRefresh != nil {
		close(stopProofRefresh)
		proofWg.Wait()
	}
	if errors.Is(err, ErrProcessIdentityMismatch) {
		summary, preview, _, _ := summarizeOutputFile(job.OutputFile, job.Cwd)
		job.markUnknown("process identity changed unexpectedly during observation (possible PID reuse)", summary, preview)
		return
	}

	deadlineStopMu.Lock()
	isTimedOut := timedOut
	stopErr := deadlineStopErr
	deadlineStopMu.Unlock()

	if errors.Is(context.Cause(ctx), ErrExecutorClosed) {
		err = ErrExecutorClosed
		exitCode = -1
		exitKnown = false
	} else if isTimedOut {
		err = &timeoutError{timeout: max(0, snap.Deadline.Sub(snap.Started))}
		exitCode = -1
		exitKnown = false
	} else if exitKnown || errors.Is(err, errAdoptedProcessExited) {
		if !exitKnown && err == nil {
			err = errAdoptedProcessExited
		}
	} else {
		waitErr := err
		if stopErr != nil {
			waitErr = stopErr
		}
		if waitErr == nil {
			waitErr = errors.New("adopted process wait failed")
		}
		info, ierr := inspectProcessErr(snap.Identity.PID)
		if ierr == nil && !info.Alive {
			// Leader has already exited; preserve unavailable-exit sentinel.
			err = errAdoptedProcessExited
			exitCode = -1
			exitKnown = false
		} else if ierr == nil && info.Alive && !snap.Identity.Matches(info) {
			summary, preview, _, _ := summarizeOutputFile(job.OutputFile, job.Cwd)
			job.markUnknown("process identity changed unexpectedly during observation (possible PID reuse)", summary, preview)
			return
		} else {
			// Leader is still alive or inspection failed with a transient error (such as EMFILE/ENFILE).
			// Keep WaitError visible and continue supervising via fallback polling so the child group
			// is not mistakenly killed under descriptor exhaustion.
			if ierr == nil && snap.Identity.Matches(info) {
				job.proofFDMu.Lock()
				job.refreshVerifiedGroupMembersLocked()
				job.proofFDMu.Unlock()
			}
			job.mu.Lock()
			if !job.finished && !job.unknown {
				job.WaitError = waitErr.Error()
				job.RecoveryNote = fmt.Sprintf("adopted live OS process PID %d (pgid %d); process wait error (%v) — supervising via fallback polling; %s",
					snap.Identity.PID, snap.Identity.PGID, waitErr, pipeCaptureEndedNote(job.OutputFile))
			}
			job.mu.Unlock()
			job.persist()

			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			leaderExited := false
			for !leaderExited {
				select {
				case <-ctx.Done():
					leaderExited = true
				case <-ticker.C:
					pinfo, perr := inspectProcessErr(snap.Identity.PID)
					if isFDLimitErr(perr) {
						// Descriptor exhaustion: cannot inspect /proc safely. Retry on next tick;
						// do not assume process exited.
						continue
					}
					if perr == nil && !pinfo.Alive {
						// Positively established exit.
						leaderExited = true
					} else if perr == nil && pinfo.Alive && !snap.Identity.Matches(pinfo) {
						summary, preview, _, _ := summarizeOutputFile(job.OutputFile, job.Cwd)
						job.markUnknown("process identity changed unexpectedly during observation (possible PID reuse)", summary, preview)
						return
					} else if perr == nil && pinfo.Alive {
						job.proofFDMu.Lock()
						job.refreshVerifiedGroupMembersLocked()
						job.proofFDMu.Unlock()
					}
				}
			}

			// Select terminal result once after fallback polling ends.
			deadlineStopMu.Lock()
			isTimedOut = timedOut
			deadlineStopMu.Unlock()

			cause := context.Cause(ctx)
			switch {
			case errors.Is(cause, ErrExecutorClosed):
				err = ErrExecutorClosed
			case isTimedOut:
				err = &timeoutError{timeout: max(0, snap.Deadline.Sub(snap.Started))}
			case cause != nil && !errors.Is(cause, context.Canceled):
				err = cause
			case ctx.Err() != nil:
				job.mu.Lock()
				settled := job.finished || job.unknown
				job.mu.Unlock()
				if settled {
					return
				}
				err = ctx.Err()
			default:
				err = errAdoptedProcessExited
			}
			exitCode = -1
			exitKnown = false
		}
	}

	job.mu.Lock()
	pendingCleanup := job.cleanupPending
	stopDeadlineFn := job.stopDeadline
	job.mu.Unlock()
	if !isTimedOut && (pendingCleanup || stopErr != nil) && !snap.Unlimited && !snap.Deadline.IsZero() && !time.Now().Before(snap.Deadline) {
		isTimedOut = true
		err = &timeoutError{timeout: max(0, snap.Deadline.Sub(snap.Started))}
		exitCode = -1
		exitKnown = false
	}
	if isTimedOut && stopDeadlineFn != nil {
		// Post-exit deadline retry loop owns group cleanup now.
		stopDeadlineFn()
	}

	cleanupBackoff := 100 * time.Millisecond
	for job.cleanupAdoptedGroupAfterExit() {
		if job.Status() != JobRunning || !isTimedOut || errors.Is(context.Cause(ctx), ErrExecutorClosed) {
			return
		}
		timer := time.NewTimer(cleanupBackoff)
		select {
		case <-m.closeCh:
			timer.Stop()
			return
		case <-job.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		cleanupBackoff = min(cleanupBackoff*2, 2*time.Second)
	}

	note := pipeCaptureEndedNote(job.OutputFile)
	completionSummary := fmt.Sprintf("(adopted background job exited; %s)", note)
	summaryNote := note
	job.mu.Lock()
	expiredOnAdoption := job.expiredOnAdoption
	job.mu.Unlock()
	if isTimedOut && expiredOnAdoption {
		timeoutNote := fmt.Sprintf("adopted job PID %d (pgid %d) exceeded deadline (%s) while harness was down; stopped verified process group; %s",
			snap.Identity.PID, snap.Identity.PGID, snap.Deadline.UTC().Format(time.RFC3339), note)
		job.mu.Lock()
		if !job.finished && !job.unknown {
			job.RecoveryNote = timeoutNote
		}
		job.mu.Unlock()
		summaryNote = timeoutNote
		completionSummary = fmt.Sprintf("(adopted background job stopped on adoption; %s)", timeoutNote)
	}
	summary, preview := formatAdoptedSummaryPreview(job.OutputFile, job.Cwd, completionSummary, summaryNote)
	job.finishAdoptedWithAudit(JobResult{
		Output:          summary,
		Err:             err,
		ExitCode:        exitCode,
		ExitCodeUnknown: !exitKnown && !isTimedOut,
		Preview:         preview,
	}, audit, exitCode, err)
}

func (j *Job) cleanupAdoptedGroupAfterExit() bool {
	j.proofFDMu.Lock()
	locked := true
	defer func() {
		if locked {
			j.proofFDMu.Unlock()
		}
	}()
	j.mu.Lock()
	if j.finished || j.unknown {
		j.mu.Unlock()
		return false
	}
	proof := j.preparePendingPostExitCleanupLocked()
	outFile, cwd := j.OutputFile, j.Cwd
	j.mu.Unlock()

	j.persist()
	gerr := j.runStopGroupAfterExit(&proof)
	j.mergeLearnedGroupMembers(proof)
	if errors.Is(gerr, ErrProcessIdentityMismatch) {
		locked = false
		j.proofFDMu.Unlock()
		summary, preview, _, _ := summarizeOutputFile(outFile, cwd)
		j.markUnknown(fmt.Sprintf("refused to signal PID %d: %v", proof.Leader.PID, gerr), summary, preview)
		return true
	}
	if gerr != nil && !errors.Is(gerr, os.ErrProcessDone) {
		j.recordStopError(fmt.Errorf("stop remaining bash processes: %w", gerr))
		return true
	}
	return false
}

// FormatRecoveredJobsSummary formats recovered jobs concisely for terminal and
// resume display, keeping each job's running/unknown/finished state explicit.
func FormatRecoveredJobsSummary(jobs []RecoveredJob) string {
	if len(jobs) == 0 {
		return ""
	}
	var running, unknown, finished int
	for _, j := range jobs {
		switch j.Status {
		case JobRunning:
			running++
		case JobUnknown:
			unknown++
		case JobDone:
			finished++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Recovered %d background job(s) (%d running, %d unknown, %d finished):", len(jobs), running, unknown, finished)
	for _, j := range jobs {
		cmd := strings.ReplaceAll(strings.TrimSpace(j.Command), "\n", " ")
		if len(cmd) > 64 {
			cmd = cmd[:61] + "..."
		}
		fmt.Fprintf(&b, "\n  - %s  status=%s", j.ID, j.Status)
		if j.PID > 0 {
			fmt.Fprintf(&b, "  pid=%d", j.PID)
		}
		if j.Adopted {
			b.WriteString(" (adopted)")
		}
		if j.ExitCode != nil {
			fmt.Fprintf(&b, "  exit=%d", *j.ExitCode)
		}
		if j.Cwd != "" {
			fmt.Fprintf(&b, "  cwd=%s", j.Cwd)
		}
		if j.OutputFile != "" {
			fmt.Fprintf(&b, "  output=%s", j.OutputFile)
		}
		if cmd != "" {
			fmt.Fprintf(&b, "  cmd=%q", cmd)
		}
		if j.Reason != "" {
			fmt.Fprintf(&b, "  (%s)", j.Reason)
		}
	}
	return b.String()
}

// StopJob explicitly cancels a running background job (or rejects safely when its
// status is unknown) and waits briefly for confirmation.
func (e *Executor) StopJob(id string) (ToolResult, error) {
	return e.killJob(id)
}

// OrphanedSessionJobs describes verified live background jobs belonging to
// another session of the same workspace that currently has no supervisor.
type OrphanedSessionJobs struct {
	SessionID string         `json:"session_id"`
	Jobs      []RecoveredJob `json:"jobs"`
}

// FindOrphanedRunningJobs inspects jobsDir read-only and returns records whose
// status is running and whose OS process identity matches a live process.
func FindOrphanedRunningJobs(jobsDir, outputID, outputDir string) ([]RecoveredJob, error) {
	records, err := loadPersistedJobs(jobsDir)
	if err != nil {
		return nil, err
	}
	var out []RecoveredJob
	for _, rec := range records {
		if rec.Status != JobRunning {
			continue
		}
		id := ProcessIdentityInfo{PID: rec.PID, PGID: rec.PGID, StartToken: rec.ProcessStarted}
		outFile := rec.OutputFile
		if !validRecoveredOutputPath(outFile, rec.OutputID, outputID, outputDir) {
			outFile = ""
		}
		if !id.Matches(inspectProcess(rec.PID)) {
			if !id.Valid() || rec.GroupProof == nil || rec.GroupProof.VerifiedAt.IsZero() {
				continue
			}
			proof := groupProofFromPersisted(rec, id, outFile)
			liveMember, scanErr := hasVerifiedLiveGroupMember(proof)
			if !liveMember && (scanErr == nil || errors.Is(scanErr, ErrProcessIdentityMismatch)) {
				continue
			}
		}
		out = append(out, RecoveredJob{
			ID:         rec.ID,
			Command:    rec.Command,
			Cwd:        rec.Cwd,
			OutputFile: outFile,
			Status:     JobRunning,
			PID:        rec.PID,
			PGID:       rec.PGID,
		})
	}
	return out, nil
}

// FormatOrphanedJobsNotice formats a startup / /jobs notice naming sessions
// with orphaned live background jobs so the user knows which session to resume.
func FormatOrphanedJobsNotice(orphans []OrphanedSessionJobs) string {
	if len(orphans) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Orphaned running background job(s) from other session(s) in this workspace:")
	for _, sess := range orphans {
		for _, j := range sess.Jobs {
			cmd := strings.ReplaceAll(strings.TrimSpace(j.Command), "\n", " ")
			if len(cmd) > 64 {
				cmd = cmd[:61] + "..."
			}
			fmt.Fprintf(&b, "\n  - session %s: %s  pid=%d", sess.SessionID, j.ID, j.PID)
			if cmd != "" {
				fmt.Fprintf(&b, "  cmd=%q", cmd)
			}
			fmt.Fprintf(&b, "  (resume with --resume %s or /resume %s to monitor or stop)", sess.SessionID, sess.SessionID)
		}
	}
	return b.String()
}
