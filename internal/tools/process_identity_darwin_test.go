//go:build darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinInspectProcessErrPreservesSysctlErrorDuringWaitRecovery(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-inspect-err"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")

	cmd, _ := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "READY",
		"GMH_HELPER_CMD":       "printf 'READY\\n'; sleep 30",
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// Pre-read persisted PID so the sysctl hook only injects EPERM for the
	// adopted job's PID during wait-error fallback inspection.
	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	targetPID := recs[0].PID
	origSysctl := darwinSysctlKinfoProcSliceFn
	var forceInspectErr atomic.Bool

	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		if forceInspectErr.Load() && name == "kern.proc.pid" && len(args) == 1 && args[0] == targetPID {
			return nil, unix.EPERM
		}
		return origSysctl(name, args...)
	}

	exec2 := &Executor{
		Root:      root,
		OutputDir: outDir,
		OutputID:  outID,
		JobsDir:   jobsDir,
		waitAdopted: func(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
			forceInspectErr.Store(true)
			return -1, false, errors.New("injected kevent failure")
		},
	}
	var recoveredJob *Job
	t.Cleanup(func() {
		forceInspectErr.Store(false)
		_ = exec2.CloseJobs(context.Background())
		if recoveredJob != nil && recoveredJob.monitorDone != nil {
			<-recoveredJob.monitorDone
		}
		darwinSysctlKinfoProcSliceFn = origSysctl
	})
	exec2.SwitchSession(outID)

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v", recovered, err)
	}
	job := exec2.registry().Get(recovered[0].ID)
	recoveredJob = job
	if job == nil {
		t.Fatal("missing recovered job")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := job.Snapshot()
		if snap.Status != JobRunning {
			t.Fatalf("job status transitioned to %q while leader inspection failed; want JobRunning", snap.Status)
		}
		if strings.Contains(snap.WaitError, "injected kevent failure") {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if snap := job.Snapshot(); snap.Status != JobRunning || !strings.Contains(snap.WaitError, "injected kevent failure") {
		t.Fatalf("snapshot = %+v, want JobRunning with WaitError preserved", snap)
	}

	forceInspectErr.Store(false)
	info, ierr := inspectProcessErr(targetPID)
	if ierr != nil || !info.Alive {
		t.Fatalf("expected live target PID %d to remain running, got info=%+v err=%v", targetPID, info, ierr)
	}

	if _, err := exec2.StopJob(job.ID); err != nil {
		t.Fatalf("StopJob failed: %v", err)
	}
	if job.Status() != JobDone {
		t.Fatalf("job status after StopJob = %q, want done", job.Status())
	}
	for wait := 0; wait < 20 && inspectProcess(targetPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(targetPID).Alive {
		t.Fatalf("target pid %d still alive after StopJob", targetPID)
	}
}

func TestDarwinNativeWaitCapturesGroupProofAfterInitialInspectError(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-native-wait-proof"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	childPIDFile := filepath.Join(root, "child.pid")
	relFile := filepath.Join(root, "release")

	cmd, _ := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "READY",
		"GMH_HELPER_CMD":       fmt.Sprintf("sleep 30 & echo $! > %q; printf 'READY\\n'; while [ ! -f %q ]; do sleep 0.02; done; exit 0", childPIDFile, relFile),
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	pidData, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child pid %q: %v", string(pidData), err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})

	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	legacy := recs[0]
	legacy.GroupProof = nil
	legacy.JobToken = ""
	if err := savePersistedJob(jobsDir, legacy); err != nil {
		t.Fatal(err)
	}
	targetPID := legacy.PID

	origSysctl := darwinSysctlKinfoProcSliceFn
	var remainingEPERM atomic.Int32
	remainingEPERM.Store(1) // one-time EPERM on initial resolveRecoveredJob lookup
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		if name == "kern.proc.pid" && len(args) == 1 && args[0] == targetPID && remainingEPERM.Add(-1) >= 0 {
			return nil, unix.EPERM
		}
		return origSysctl(name, args...)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	var recoveredJob *Job
	t.Cleanup(func() {
		remainingEPERM.Store(0)
		_ = exec2.CloseJobs(context.Background())
		if recoveredJob != nil && recoveredJob.monitorDone != nil {
			<-recoveredJob.monitorDone
		}
		darwinSysctlKinfoProcSliceFn = origSysctl
	})
	exec2.SwitchSession(outID)

	rec, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(rec) != 1 || rec[0].Status != JobRunning {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v; want JobRunning", rec, err)
	}
	job := exec2.registry().Get(rec[0].ID)
	recoveredJob = job

	// Wait for the native-wait pre-refresh to capture VerifiedAt before leader exits.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job.mu.Lock()
		verified := !job.groupProof.VerifiedAt.IsZero()
		job.mu.Unlock()
		if verified {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if err := os.WriteFile(relFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-job.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("adopted job did not finish after leader exit")
	}
	if job.Status() != JobDone {
		t.Fatalf("job status = %q, want done (note=%q)", job.Status(), job.Snapshot().RecoveryNote)
	}
	for wait := 0; wait < 20 && inspectProcess(childPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childPID).Alive {
		t.Fatalf("surviving child pid %d stayed alive after leader exit", childPID)
	}
}

func TestDarwinLiveAdoptionPreservesPersistedMembersWhenRecaptureFails(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-preserve-members"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	childPIDFile := filepath.Join(root, "child.pid")
	relFile := filepath.Join(root, "release")

	cmd, _ := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "READY",
		"GMH_HELPER_CMD":       fmt.Sprintf("sleep 30 & echo $! > %q; printf 'READY\\n'; while [ ! -f %q ]; do sleep 0.02; done; exit 0", childPIDFile, relFile),
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	pidData, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child pid %q: %v", string(pidData), err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})

	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	rec := recs[0]
	childInfo := inspectProcess(childPID)
	if !childInfo.Alive {
		t.Fatalf("child pid %d not alive", childPID)
	}
	rec.GroupProof = &PersistedGroupProof{
		UID:          childInfo.UID,
		HasUID:       childInfo.HasUID,
		SID:          childInfo.SID,
		Sess:         uint64(childInfo.Sess),
		VerifiedAt:   time.Now(),
		KnownMembers: map[int]string{rec.PID: rec.ProcessStarted, childPID: childInfo.StartToken},
	}
	if err := savePersistedJob(jobsDir, rec); err != nil {
		t.Fatal(err)
	}

	origSlice := darwinSysctlKinfoProcSliceFn
	var failPgrpOnce atomic.Bool
	var pgrpFaultFired atomic.Bool
	failPgrpOnce.Store(true)
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		if name == "kern.proc.pgrp" && failPgrpOnce.Swap(false) {
			pgrpFaultFired.Store(true)
			return nil, unix.EPERM
		}
		return origSlice(name, args...)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	var recoveredJob *Job
	t.Cleanup(func() {
		failPgrpOnce.Store(false)
		_ = exec2.CloseJobs(context.Background())
		if recoveredJob != nil && recoveredJob.monitorDone != nil {
			<-recoveredJob.monitorDone
		}
		darwinSysctlKinfoProcSliceFn = origSlice
	})
	exec2.SwitchSession(outID)

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v", recovered, err)
	}
	if !pgrpFaultFired.Load() {
		t.Fatal("expected kern.proc.pgrp EPERM fault to fire during recapture")
	}
	job := exec2.registry().Get(recovered[0].ID)
	recoveredJob = job
	job.mu.Lock()
	preservedTok := job.groupProof.KnownMembers[childPID]
	job.mu.Unlock()
	if preservedTok != childInfo.StartToken {
		t.Fatalf("persisted child token lost on recapture failure: got %q, want %q", preservedTok, childInfo.StartToken)
	}
	persisted, err := loadPersistedJobs(jobsDir)
	if err != nil || len(persisted) != 1 || persisted[0].GroupProof == nil || persisted[0].GroupProof.KnownMembers[childPID] != childInfo.StartToken {
		t.Fatalf("persisted GroupProof lost child member after recapture failure: %+v", persisted)
	}
	if err := os.WriteFile(relFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("adopted job did not finish")
	}
	for wait := 0; wait < 20 && inspectProcess(childPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childPID).Alive {
		t.Fatalf("child pid %d survived leader exit after recapture failure", childPID)
	}
}

func TestDarwinExpiredDeadlineDuringDowntimeReportsExpiryAfterInspectRecovery(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-expired-inspect"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")

	cmd, _ := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "READY",
		"GMH_HELPER_CMD":       "printf 'READY\\n'; sleep 30",
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	expired := recs[0]
	expired.Unlimited = false
	expired.Deadline = time.Now().Add(-10 * time.Second)
	if err := savePersistedJob(jobsDir, expired); err != nil {
		t.Fatal(err)
	}
	targetPID := expired.PID

	origSlice := darwinSysctlKinfoProcSliceFn
	var remainingEPERM atomic.Int32
	remainingEPERM.Store(1)
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		if name == "kern.proc.pid" && len(args) == 1 && args[0] == targetPID && remainingEPERM.Add(-1) >= 0 {
			return nil, unix.EPERM
		}
		return origSlice(name, args...)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	var recoveredJob *Job
	t.Cleanup(func() {
		remainingEPERM.Store(0)
		_ = exec2.CloseJobs(context.Background())
		if recoveredJob != nil && recoveredJob.monitorDone != nil {
			<-recoveredJob.monitorDone
		}
		darwinSysctlKinfoProcSliceFn = origSlice
	})
	exec2.SwitchSession(outID)

	rec, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(rec) != 1 {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v", rec, err)
	}
	job := exec2.registry().Get(rec[0].ID)
	recoveredJob = job
	select {
	case <-job.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("expired job did not finish after transient inspect error cleared")
	}
	snap := job.Snapshot()
	if snap.Status != JobDone || !strings.Contains(snap.RecoveryNote, "exceeded deadline") || !strings.Contains(snap.RecoveryNote, "while harness was down") {
		t.Fatalf("RecoveryNote = %q (status=%q), want downtime deadline expiry note", snap.RecoveryNote, snap.Status)
	}
	if strings.Contains(snap.RecoveryNote, "job remains stoppable") {
		t.Fatalf("RecoveryNote still says stale 'job remains stoppable': %q", snap.RecoveryNote)
	}
}

func TestDarwinPendingPostExitCleanupReportsDowntimeDeadlineExpiry(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		name := "immediate_cleanup"
		if failFirst {
			name = "retry_after_transient_cleanup_error"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			sessionBase := t.TempDir()
			outID := "sess-darwin-pending-expired-" + name
			outDir := filepath.Join(sessionBase, outID, "bash-output")
			jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
			childPIDFile := filepath.Join(root, "child.pid")

			cmd, _ := spawnCrashHelper(t, map[string]string{
				"GMH_HELPER_ROOT":      root,
				"GMH_HELPER_OUTDIR":    outDir,
				"GMH_HELPER_JOBSDIR":   jobsDir,
				"GMH_HELPER_OUTID":     outID,
				"GMH_HELPER_WAIT_TEXT": "READY",
				"GMH_HELPER_CMD":       fmt.Sprintf("sleep 30 & echo $! > %q; printf 'READY\\n'; exit 0", childPIDFile),
			})
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()

			pidData, err := os.ReadFile(childPIDFile)
			if err != nil {
				t.Fatal(err)
			}
			childPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil || childPID <= 0 {
				t.Fatalf("invalid child pid %q: %v", string(pidData), err)
			}
			t.Cleanup(func() {
				if p, err := os.FindProcess(childPID); err == nil {
					_ = p.Kill()
				}
			})

			recs, err := loadPersistedJobs(jobsDir)
			if err != nil || len(recs) != 1 {
				t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
			}
			rec := recs[0]
			childInfo := inspectProcess(childPID)
			if !childInfo.Alive {
				t.Fatalf("child pid %d not alive", childPID)
			}
			rec.Unlimited = false
			rec.Deadline = time.Now().Add(-10 * time.Second)
			rec.GroupProof = &PersistedGroupProof{
				UID:            childInfo.UID,
				HasUID:         childInfo.HasUID,
				SID:            childInfo.SID,
				Sess:           uint64(childInfo.Sess),
				VerifiedAt:     time.Now().Add(-20 * time.Second),
				CleanupPending: true,
				KnownMembers:   map[int]string{rec.PID: rec.ProcessStarted, childPID: childInfo.StartToken},
			}
			if err := savePersistedJob(jobsDir, rec); err != nil {
				t.Fatal(err)
			}

			// Wait until the leader exits while the verified child remains alive.
			for wait := 0; wait < 40 && inspectProcess(rec.PID).Alive; wait++ {
				time.Sleep(15 * time.Millisecond)
			}

			var cleanupCalls atomic.Int32
			exec2 := &Executor{
				Root:      root,
				OutputDir: outDir,
				OutputID:  outID,
				JobsDir:   jobsDir,
				stopAdoptedGroupAfterExit: func(proof processGroupProof) error {
					if failFirst && cleanupCalls.Add(1) == 1 {
						return errors.New("transient cleanup failure")
					}
					return stopAdoptedProcessGroupAfterExit(proof)
				},
			}
			t.Cleanup(func() { _ = exec2.CloseJobs(context.Background()) })
			exec2.SwitchSession(outID)

			recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
			if err != nil || len(recovered) != 1 {
				t.Fatalf("RecoverSessionJobs = %+v, err=%v", recovered, err)
			}
			job := exec2.registry().Get(recovered[0].ID)
			select {
			case <-job.Done():
			case <-time.After(4 * time.Second):
				t.Fatal("pending post-exit expired job did not settle")
			}
			snap := job.Snapshot()
			if snap.Status != JobDone {
				t.Fatalf("status = %q, want done", snap.Status)
			}
			if !strings.Contains(snap.RecoveryNote, "exceeded deadline") || !strings.Contains(snap.RecoveryNote, "while harness was down") {
				t.Fatalf("RecoveryNote = %q, want downtime deadline expiry note", snap.RecoveryNote)
			}
			if strings.Contains(snap.RecoveryNote, "job remains stoppable") {
				t.Fatalf("RecoveryNote retained stale 'job remains stoppable': %q", snap.RecoveryNote)
			}
		})
	}
}

func TestDarwinInitialRecoveryPreservesLegacyJobOnInspectError(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-initial-inspect-err"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")

	cmd, _ := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "READY",
		"GMH_HELPER_CMD":       "printf 'READY\\n'; sleep 30",
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// Remove persisted GroupProof and JobToken to model a legacy record.
	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	legacy := recs[0]
	legacy.GroupProof = nil
	legacy.JobToken = ""
	if err := savePersistedJob(jobsDir, legacy); err != nil {
		t.Fatalf("savePersistedJob: %v", err)
	}
	targetPID := legacy.PID

	origSysctl := darwinSysctlKinfoProcSliceFn
	var forceInspectErr atomic.Bool
	forceInspectErr.Store(true)
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		if forceInspectErr.Load() && name == "kern.proc.pid" && len(args) == 1 && args[0] == targetPID {
			return nil, unix.EPERM
		}
		return origSysctl(name, args...)
	}

	exec2 := &Executor{
		Root:      root,
		OutputDir: outDir,
		OutputID:  outID,
		JobsDir:   jobsDir,
	}
	var recoveredJob *Job
	t.Cleanup(func() {
		forceInspectErr.Store(false)
		_ = exec2.CloseJobs(context.Background())
		if recoveredJob != nil && recoveredJob.monitorDone != nil {
			<-recoveredJob.monitorDone
		}
		darwinSysctlKinfoProcSliceFn = origSysctl
	})
	exec2.SwitchSession(outID)

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v", recovered, err)
	}
	if recovered[0].Status != JobRunning {
		t.Fatalf("recovered legacy job status = %q during transient EPERM, want JobRunning", recovered[0].Status)
	}
	job := exec2.registry().Get(recovered[0].ID)
	recoveredJob = job
	if job == nil || job.monitorDone == nil {
		t.Fatalf("expected monitor to be attached on transient initial inspect error")
	}

	persistedAfter, err := loadPersistedJobs(jobsDir)
	if err != nil || len(persistedAfter) != 1 || persistedAfter[0].Status != JobRunning {
		t.Fatalf("persisted record overwritten during transient inspect error: %+v, err=%v", persistedAfter, err)
	}

	// Clear transient inspection error; fallback polling and StopJob must now work cleanly.
	forceInspectErr.Store(false)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := job.Snapshot()
		if snap.Status != JobRunning {
			t.Fatalf("job status changed unexpectedly to %q", snap.Status)
		}
		job.mu.Lock()
		verified := !job.groupProof.VerifiedAt.IsZero()
		job.mu.Unlock()
		if verified {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := exec2.StopJob(job.ID); err != nil {
		t.Fatalf("StopJob failed after inspect error cleared: %v", err)
	}
	if job.Status() != JobDone {
		t.Fatalf("job status after StopJob = %q, want done", job.Status())
	}
	for wait := 0; wait < 20 && inspectProcess(targetPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(targetPID).Alive {
		t.Fatalf("target pid %d still alive after StopJob", targetPID)
	}
}
