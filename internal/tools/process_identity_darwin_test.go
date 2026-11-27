//go:build darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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

func TestDarwinAccessDeniedCleanupSignalsVerifiedChildrenAndKeepsLeaderPending(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-eacces-descendants"
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
	// Trigger expired deadline during downtime so initial recovery hits leader EACCES.
	rec.Unlimited = false
	rec.Deadline = time.Now().Add(-10 * time.Second)
	if err := savePersistedJob(jobsDir, rec); err != nil {
		t.Fatal(err)
	}
	targetPID := rec.PID

	// Release leader so only the verified child remains in the PGID while leader inspect denies access.
	if err := os.WriteFile(relFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for wait := 0; wait < 40 && inspectProcess(targetPID).Alive; wait++ {
		time.Sleep(15 * time.Millisecond)
	}

	origInspect := inspectProcessErrFn
	var forceLeaderEACCES atomic.Bool
	forceLeaderEACCES.Store(true)
	inspectProcessErrFn = func(pid int) (processInfo, error) {
		if forceLeaderEACCES.Load() && pid == targetPID {
			return processInfo{Alive: true, PGID: targetPID, StartToken: rec.ProcessStarted, UID: childInfo.UID, HasUID: true, SID: childInfo.SID, Sess: childInfo.Sess}, nil
		}
		return origInspect(pid)
	}

	origStopGroup := stopVerifiedProcessGroupFn
	stopVerifiedProcessGroupFn = func(proof processGroupProof) error {
		if forceLeaderEACCES.Load() && proof.Leader.PID == targetPID {
			return syscall.EACCES
		}
		return origStopGroup(proof)
	}

	// Switch inspectProcessErrFn to return EACCES right as stopAdoptedProcessGroupAfterExitTrack inspects targetPID.
	var callCount atomic.Int32
	inspectProcessErrFn = func(pid int) (processInfo, error) {
		if forceLeaderEACCES.Load() && pid == targetPID {
			if callCount.Add(1) == 1 {
				return processInfo{Alive: true, PGID: targetPID, StartToken: rec.ProcessStarted, UID: childInfo.UID, HasUID: true, SID: childInfo.SID, Sess: childInfo.Sess}, nil
			}
			return processInfo{}, syscall.EACCES
		}
		return origInspect(pid)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	var recoveredJob *Job
	t.Cleanup(func() {
		forceLeaderEACCES.Store(false)
		inspectProcessErrFn = origInspect
		stopVerifiedProcessGroupFn = origStopGroup
		_ = exec2.CloseJobs(context.Background())
		if recoveredJob != nil && recoveredJob.monitorDone != nil {
			<-recoveredJob.monitorDone
		}
	})
	exec2.SwitchSession(outID)

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v", recovered, err)
	}
	recoveredJob = exec2.registry().Get(recovered[0].ID)

	// Verified child must be cleaned even while leader EACCES keeps job unresolved/running.
	for wait := 0; wait < 40 && inspectProcess(childPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childPID).Alive {
		t.Fatalf("verified child pid %d survived cleanup when leader inspect returned EACCES", childPID)
	}
	if recoveredJob.Status() != JobRunning {
		t.Fatalf("job status = %q while leader state unresolved under EACCES, want JobRunning", recoveredJob.Status())
	}

	forceLeaderEACCES.Store(false)
	select {
	case <-recoveredJob.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("job did not settle after leader EACCES cleared")
	}
	if recoveredJob.Status() != JobDone {
		t.Fatalf("status after leader inspect recovered = %q, want done", recoveredJob.Status())
	}
}

func TestDarwinAccessDeniedGroupUIDMismatchStillKillsVerifiedKnownMember(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-eacces-uid-mismatch"
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

	childInfo := inspectProcess(childPID)
	if !childInfo.Alive {
		t.Fatalf("child pid %d not alive", childPID)
	}
	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	rec := recs[0]
	leaderPID := rec.PID

	// Release leader so child stays alive alone.
	if err := os.WriteFile(relFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for wait := 0; wait < 40 && inspectProcess(leaderPID).Alive; wait++ {
		time.Sleep(15 * time.Millisecond)
	}

	origSlice := darwinSysctlKinfoProcSliceFn
	origInspect := inspectProcessErrFn
	origStopGroup := stopVerifiedProcessGroupFn
	t.Cleanup(func() {
		darwinSysctlKinfoProcSliceFn = origSlice
		inspectProcessErrFn = origInspect
		stopVerifiedProcessGroupFn = origStopGroup
	})

	// Inject a foreign UID on pgrp lookup to force darwinResolveGroupCandidates to
	// fail with UID mismatch while leader inspection returns EACCES.
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		procs, err := origSlice(name, args...)
		if name == "kern.proc.pgrp" && len(procs) > 0 {
			for i := range procs {
				procs[i].Eproc.Ucred.Uid = childInfo.UID + 1
			}
		}
		return procs, err
	}
	var leaderGone atomic.Bool
	inspectProcessErrFn = func(pid int) (processInfo, error) {
		if !leaderGone.Load() && pid == leaderPID {
			return processInfo{}, syscall.EACCES
		}
		return origInspect(pid)
	}
	stopVerifiedProcessGroupFn = func(proof processGroupProof) error {
		if !leaderGone.Load() && proof.Leader.PID == leaderPID {
			return syscall.EACCES
		}
		return os.ErrProcessDone
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	job := exec2.newRecoveredJobObject(rec, jobsDir, JobRunning, false, true, "adopted", JobResult{})
	job.groupProof = processGroupProof{
		Leader:       ProcessIdentityInfo{PID: leaderPID, PGID: rec.PGID, StartToken: rec.ProcessStarted},
		UID:          childInfo.UID,
		HasUID:       true,
		SID:          childInfo.SID,
		Sess:         childInfo.Sess,
		VerifiedAt:   time.Now().Add(-5 * time.Second),
		KnownMembers: map[int]string{leaderPID: rec.ProcessStarted, childPID: childInfo.StartToken},
	}

	stopErr := job.stopAdoptedProcess(nil, false)
	if !errors.Is(stopErr, syscall.EACCES) {
		t.Fatalf("stopAdoptedProcess err = %v, want EACCES keeping job retryable", stopErr)
	}
	if job.Status() != JobRunning {
		t.Fatalf("job status = %q after group UID mismatch under leader EACCES, want JobRunning", job.Status())
	}
	for wait := 0; wait < 20 && inspectProcess(childPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childPID).Alive {
		t.Fatalf("verified child pid %d survived Darwin cleanup after UID mismatch under leader EACCES", childPID)
	}

	// Retry after leader exits cleanly must succeed without failing on old UID mismatch.
	leaderGone.Store(true)
	if err := job.stopAdoptedProcess(nil, false); err != nil {
		t.Fatalf("follow-up stopAdoptedProcess err = %v", err)
	}
	if job.Status() != JobDone {
		t.Fatalf("status after retry = %q, want done", job.Status())
	}
}

func TestDarwinExitedLeaderUIDMismatchRecoversAndSurfacesOrphan(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-exited-uid-mismatch"
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
	childInfo := inspectProcess(childPID)
	if !childInfo.Alive {
		t.Fatalf("child pid %d not alive", childPID)
	}
	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	rec := recs[0]
	rec.GroupProof = &PersistedGroupProof{
		UID:            childInfo.UID,
		HasUID:         true,
		SID:            childInfo.SID,
		Sess:           uint64(childInfo.Sess),
		VerifiedAt:     time.Now().Add(-5 * time.Second),
		CleanupPending: false,
		KnownMembers:   map[int]string{rec.PID: rec.ProcessStarted, childPID: childInfo.StartToken},
	}
	if err := savePersistedJob(jobsDir, rec); err != nil {
		t.Fatal(err)
	}

	// Leader exits while child remains alive (no CleanupPending flag set on disk).
	if err := os.WriteFile(relFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for wait := 0; wait < 40 && inspectProcess(rec.PID).Alive; wait++ {
		time.Sleep(15 * time.Millisecond)
	}

	origSlice := darwinSysctlKinfoProcSliceFn
	t.Cleanup(func() { darwinSysctlKinfoProcSliceFn = origSlice })
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		procs, err := origSlice(name, args...)
		if name == "kern.proc.pgrp" && len(procs) > 0 {
			// Force UID mismatch in pgrp scan while kern.proc.pid remains intact.
			for i := range procs {
				procs[i].Eproc.Ucred.Uid = childInfo.UID + 1
			}
		}
		return procs, err
	}

	// 1. Orphan scan must still surface this job because verified child is live.
	orphans, err := FindOrphanedRunningJobs(jobsDir, "other-sess", "")
	if err != nil || len(orphans) != 1 {
		t.Fatalf("FindOrphanedRunningJobs = %+v, err=%v; want 1 orphan", orphans, err)
	}

	// 2. Recovery without CleanupPending must find & clean up the verified child and finish JobDone.
	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(recovered) != 1 || recovered[0].Status != JobDone {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v; want JobDone after cleaning verified child", recovered, err)
	}
	for wait := 0; wait < 20 && inspectProcess(childPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childPID).Alive {
		t.Fatalf("verified child pid %d survived recovery", childPID)
	}
}

func TestDarwinRecoveryRejectsMovedPGIDKnownMember(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-moved-pgid"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	childPIDFile := filepath.Join(root, "child.pid")
	movedMarker := filepath.Join(root, "moved")
	relFile := filepath.Join(root, "release")

	// Start a helper leader whose child stays in the same PGID briefly, then moves
	// itself to its own process group (`setpgid`) after we persist its initial identity.
	cmd, _ := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "READY",
		"GMH_HELPER_CMD":       fmt.Sprintf("python3 -c 'import os,time; open(%q,\"w\").write(str(os.getpid())); time.sleep(0.05)' & printf 'READY\\n'; while [ ! -f %q ]; do sleep 0.02; done; exit 0", childPIDFile, relFile),
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// Spawn a separate process in its own process group (still same UID/session) to
	// represent the child after changing PGID while matching recorded StartToken/UID/Sess.
	movedCmd := exec.Command("sleep", "30")
	configureProcess(movedCmd)
	if err := movedCmd.Start(); err != nil {
		t.Fatal(err)
	}
	movedPID := movedCmd.Process.Pid
	t.Cleanup(func() {
		if p, err := os.FindProcess(movedPID); err == nil {
			_ = p.Kill()
		}
		_ = movedCmd.Wait()
	})
	movedInfo := inspectProcess(movedPID)
	if !movedInfo.Alive {
		t.Fatalf("moved process %d not alive", movedPID)
	}

	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	rec := recs[0]
	leaderPID := rec.PID
	_ = movedMarker

	if err := os.WriteFile(relFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for wait := 0; wait < 40 && inspectProcess(leaderPID).Alive; wait++ {
		time.Sleep(15 * time.Millisecond)
	}

	origSlice := darwinSysctlKinfoProcSliceFn
	t.Cleanup(func() { darwinSysctlKinfoProcSliceFn = origSlice })
	// Inject a foreign-UID entry in pgrp scan while movedPID sits outside rec.PGID
	// in the same session and UID.
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		if name != "kern.proc.pgrp" {
			return origSlice(name, args...)
		}
		var intruder unix.KinfoProc
		intruder.Proc.P_pid = 999999
		intruder.Proc.P_starttime = unix.Timeval{Sec: time.Now().Unix()}
		intruder.Eproc.Pgid = int32(rec.PGID)
		intruder.Eproc.Ucred.Uid = movedInfo.UID + 1
		intruder.Eproc.Sess = movedInfo.Sess
		return []unix.KinfoProc{intruder}, nil
	}

	proof := processGroupProof{
		Leader:       ProcessIdentityInfo{PID: leaderPID, PGID: rec.PGID, StartToken: rec.ProcessStarted},
		UID:          movedInfo.UID,
		HasUID:       true,
		SID:          movedInfo.SID,
		Sess:         movedInfo.Sess,
		VerifiedAt:   time.Now().Add(-5 * time.Second),
		KnownMembers: map[int]string{leaderPID: rec.ProcessStarted, movedPID: movedInfo.StartToken},
	}

	live, lerr := hasVerifiedLiveGroupMember(proof)
	if live || !errors.Is(lerr, ErrProcessIdentityMismatch) {
		t.Fatalf("hasVerifiedLiveGroupMember = (%v, %v), want (false, ErrProcessIdentityMismatch) for moved-PGID member", live, lerr)
	}
	if err := stopAdoptedProcessGroupAfterExitTrack(&proof); !errors.Is(err, ErrProcessIdentityMismatch) {
		t.Fatalf("stopAdoptedProcessGroupAfterExitTrack = %v, want ErrProcessIdentityMismatch", err)
	}
	if !inspectProcess(movedPID).Alive {
		t.Fatalf("process %d outside job PGID was killed by fallback", movedPID)
	}
}

func TestDarwinRecoveryKeepsJobPendingWhenOneKnownMemberRaisesEACCES(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-darwin-partial-eacces"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	childAPIDFile := filepath.Join(root, "childA.pid")
	childBPIDFile := filepath.Join(root, "childB.pid")
	relFile := filepath.Join(root, "release")

	cmd, _ := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "READY",
		"GMH_HELPER_CMD":       fmt.Sprintf("sleep 30 & echo $! > %q; sleep 30 & echo $! > %q; printf 'READY\\n'; while [ ! -f %q ]; do sleep 0.02; done; exit 0", childAPIDFile, childBPIDFile, relFile),
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	readPID := func(p string) int {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		v, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || v <= 0 {
			t.Fatalf("bad pid %q: %v", string(b), err)
		}
		return v
	}
	childAPID := readPID(childAPIDFile)
	childBPID := readPID(childBPIDFile)
	t.Cleanup(func() {
		for _, pid := range []int{childAPID, childBPID} {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	})

	infoA := inspectProcess(childAPID)
	infoB := inspectProcess(childBPID)
	if !infoA.Alive || !infoB.Alive {
		t.Fatalf("children not alive: A=%+v B=%+v", infoA, infoB)
	}
	recs, err := loadPersistedJobs(jobsDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("loadPersistedJobs = %+v, err=%v", recs, err)
	}
	rec := recs[0]
	rec.GroupProof = &PersistedGroupProof{
		UID:            infoA.UID,
		HasUID:         true,
		SID:            infoA.SID,
		Sess:           uint64(infoA.Sess),
		VerifiedAt:     time.Now().Add(-5 * time.Second),
		CleanupPending: true,
		KnownMembers: map[int]string{
			rec.PID:   rec.ProcessStarted,
			childAPID: infoA.StartToken,
			childBPID: infoB.StartToken,
		},
	}
	if err := savePersistedJob(jobsDir, rec); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(relFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for wait := 0; wait < 40 && inspectProcess(rec.PID).Alive; wait++ {
		time.Sleep(15 * time.Millisecond)
	}

	origSlice := darwinSysctlKinfoProcSliceFn
	origInspect := inspectProcessErrFn
	var forceBEACCES atomic.Bool
	forceBEACCES.Store(true)
	t.Cleanup(func() {
		forceBEACCES.Store(false)
		darwinSysctlKinfoProcSliceFn = origSlice
		inspectProcessErrFn = origInspect
	})
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		procs, err := origSlice(name, args...)
		if name == "kern.proc.pgrp" && len(procs) > 0 {
			for i := range procs {
				procs[i].Eproc.Ucred.Uid = infoA.UID + 1
			}
		}
		return procs, err
	}
	inspectProcessErrFn = func(pid int) (processInfo, error) {
		if forceBEACCES.Load() && pid == childBPID {
			return processInfo{}, syscall.EACCES
		}
		return origInspect(pid)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("RecoverSessionJobs = %+v, err=%v", recovered, err)
	}
	if recovered[0].Status != JobRunning {
		t.Fatalf("recovered status = %q while childB returned EACCES, want JobRunning (must not mark JobDone)", recovered[0].Status)
	}
	for wait := 0; wait < 20 && inspectProcess(childAPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childAPID).Alive {
		t.Fatalf("childA pid %d survived cleanup", childAPID)
	}
	if !inspectProcess(childBPID).Alive {
		t.Fatalf("expected childB pid %d to still be alive under injected EACCES", childBPID)
	}

	forceBEACCES.Store(false)
	if _, err := exec2.StopJob(recovered[0].ID); err != nil {
		t.Fatalf("StopJob retry after childB EACCES cleared failed: %v", err)
	}
	for wait := 0; wait < 20 && inspectProcess(childBPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childBPID).Alive {
		t.Fatalf("childB pid %d survived StopJob retry", childBPID)
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

func TestDarwinMismatchRetryTracksNewlyVerifiedChildAndKeepsPending(t *testing.T) {
	tmpDir := t.TempDir()
	childAPIDFile := filepath.Join(tmpDir, "childA.pid")
	childCPIDFile := filepath.Join(tmpDir, "childC.pid")
	// Spawn one bash parent childA (pid written to childAPIDFile) that spawns
	// childC (pid written to childCPIDFile) while childA waits, keeping
	// childA alive as childC's verified parent after groupCmd exits.
	groupCmd := exec.Command("bash", "-c", fmt.Sprintf(
		"bash -c 'sleep 30 & echo $! > %q; wait' & echo $! > %q",
		childCPIDFile, childAPIDFile,
	))
	configureProcess(groupCmd)
	if err := groupCmd.Start(); err != nil {
		t.Fatal(err)
	}
	leaderPID := groupCmd.Process.Pid
	leaderInfo := inspectProcess(leaderPID)
	readPID := func(path string) int {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(path); err == nil {
				if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && p > 0 {
					return p
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for pid file %s", path)
		return 0
	}
	childAPID := readPID(childAPIDFile)
	childCPID := readPID(childCPIDFile)
	_ = groupCmd.Wait()
	t.Cleanup(func() {
		for _, pid := range []int{childAPID, childCPID} {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	infoA := inspectProcess(childAPID)
	infoC := inspectProcess(childCPID)
	if !infoA.Alive || !infoC.Alive {
		t.Fatalf("children not alive: A=%+v C=%+v", infoA, infoC)
	}
	if infoA.PGID != infoC.PGID {
		t.Fatalf("expected shared PGID: A=%+v C=%+v", infoA, infoC)
	}

	intruderCmd := exec.Command("sleep", "30")
	configureProcess(intruderCmd)
	if err := intruderCmd.Start(); err != nil {
		t.Fatal(err)
	}
	intruderPID := intruderCmd.Process.Pid
	t.Cleanup(func() {
		if p, err := os.FindProcess(intruderPID); err == nil {
			_ = p.Kill()
		}
		_ = intruderCmd.Wait()
	})
	intruderSlice, err := unix.SysctlKinfoProcSlice("kern.proc.pid", intruderPID)
	if err != nil || len(intruderSlice) == 0 {
		t.Fatalf("sysctl intruder pid %d failed: %v", intruderPID, err)
	}
	intruderProc := intruderSlice[0]
	intruderProc.Eproc.Ucred.Uid = infoA.UID + 1
	intruderProc.Eproc.Pgid = int32(infoA.PGID)

	origSlice := darwinSysctlKinfoProcSliceFn
	origInspect := inspectProcessErrFn
	t.Cleanup(func() {
		darwinSysctlKinfoProcSliceFn = origSlice
		inspectProcessErrFn = origInspect
	})

	var blockC atomic.Bool
	blockC.Store(true)
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		procs, err := origSlice(name, args...)
		if err != nil {
			return nil, err
		}
		if name == "kern.proc.pgrp" && len(args) == 1 && args[0] == infoA.PGID {
			// Put mismatched intruder BEFORE real members so the scan hits a
			// mismatch before reaching newly forked childC.
			if inspectProcess(intruderPID).Alive {
				return append([]unix.KinfoProc{intruderProc}, procs...), nil
			}
		}
		return procs, nil
	}
	inspectProcessErrFn = func(pid int) (processInfo, error) {
		if pid == leaderPID {
			return processInfo{}, nil
		}
		if pid == childCPID && blockC.Load() {
			return processInfo{}, syscall.EACCES
		}
		return origInspect(pid)
	}

	leaderTok := leaderInfo.StartToken
	if leaderTok == "" {
		leaderTok = infoA.StartToken
	}
	proof := processGroupProof{
		Leader:         ProcessIdentityInfo{PID: leaderPID, PGID: infoA.PGID, StartToken: leaderTok},
		UID:            infoA.UID,
		HasUID:         true,
		SID:            infoA.SID,
		Sess:           infoA.Sess,
		VerifiedAt:     time.Now().Add(-5 * time.Second),
		ExitObservedAt: time.Now().Add(-2 * time.Second),
		KnownMembers: map[int]string{
			leaderPID: leaderTok,
			childAPID: infoA.StartToken,
		},
	}

	if err := stopAdoptedProcessGroupAfterExitTrack(&proof); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("stopAdoptedProcessGroupAfterExitTrack err = %v, want EACCES while newly verified childC cannot be inspected", err)
	}
	if proof.KnownMembers[childCPID] != infoC.StartToken {
		t.Fatalf("expected newly verified childC (%d) retained in proof.KnownMembers: %+v", childCPID, proof.KnownMembers)
	}
	for wait := 0; wait < 20 && inspectProcess(childAPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childAPID).Alive {
		t.Fatalf("childA pid %d survived cleanup", childAPID)
	}
	if !inspectProcess(childCPID).Alive {
		t.Fatalf("childC pid %d should remain alive under EACCES", childCPID)
	}
	if !inspectProcess(intruderPID).Alive {
		t.Fatalf("intruder pid %d was killed unexpectedly", intruderPID)
	}

	// Clearing EACCES and retrying finishes stopping newly verified childC cleanly.
	blockC.Store(false)
	if rerr := stopAdoptedProcessGroupAfterExitTrack(&proof); rerr != nil {
		t.Fatalf("retry stopAdoptedProcessGroupAfterExitTrack err = %v, want nil", rerr)
	}
	for wait := 0; wait < 20 && inspectProcess(childCPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childCPID).Alive {
		t.Fatalf("newly verified childC pid %d survived mismatch retry", childCPID)
	}
	if !inspectProcess(intruderPID).Alive {
		t.Fatalf("intruder pid %d was killed unexpectedly on retry", intruderPID)
	}
}

func TestDarwinLateUnverifiedMemberStillCleansVerifiedSiblings(t *testing.T) {
	tmpDir := t.TempDir()
	childPIDFile := filepath.Join(tmpDir, "child.pid")
	leaderCmd := exec.Command("bash", "-c", fmt.Sprintf("sleep 30 & echo $! > %q", childPIDFile))
	configureProcess(leaderCmd)
	if err := leaderCmd.Start(); err != nil {
		t.Fatal(err)
	}
	leaderPID := leaderCmd.Process.Pid
	leaderInfo := inspectProcess(leaderPID)
	var childPID int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(childPIDFile); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && p > 0 {
				childPID = p
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("child did not start")
	}
	_ = leaderCmd.Wait()
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})
	childInfo := inspectProcess(childPID)
	if !childInfo.Alive {
		t.Fatalf("child pid %d not alive", childPID)
	}

	// Spawn a separate process injected into kern.proc.pgrp whose start time is
	// after ExitObservedAt + 10ms and without KnownMembers/parent ancestry.
	lateCmd := exec.Command("sleep", "30")
	configureProcess(lateCmd)
	if err := lateCmd.Start(); err != nil {
		t.Fatal(err)
	}
	latePID := lateCmd.Process.Pid
	t.Cleanup(func() {
		if p, err := os.FindProcess(latePID); err == nil {
			_ = p.Kill()
		}
		_ = lateCmd.Wait()
	})
	lateSlice, err := unix.SysctlKinfoProcSlice("kern.proc.pid", latePID)
	if err != nil || len(lateSlice) == 0 {
		t.Fatalf("sysctl late pid %d failed: %v", latePID, err)
	}
	lateProc := lateSlice[0]
	lateProc.Eproc.Pgid = int32(childInfo.PGID)
	lateProc.Eproc.Ucred.Uid = childInfo.UID
	lateProc.Eproc.Sess = childInfo.Sess

	origSlice := darwinSysctlKinfoProcSliceFn
	t.Cleanup(func() { darwinSysctlKinfoProcSliceFn = origSlice })
	darwinSysctlKinfoProcSliceFn = func(name string, args ...int) ([]unix.KinfoProc, error) {
		procs, err := origSlice(name, args...)
		if err != nil {
			return nil, err
		}
		if name == "kern.proc.pgrp" && len(args) == 1 && args[0] == childInfo.PGID && inspectProcess(latePID).Alive {
			return append(procs, lateProc), nil
		}
		return procs, nil
	}

	leaderTok := leaderInfo.StartToken
	if leaderTok == "" {
		leaderTok = childInfo.StartToken
	}
	proof := processGroupProof{
		Leader:         ProcessIdentityInfo{PID: leaderPID, PGID: childInfo.PGID, StartToken: leaderTok},
		UID:            childInfo.UID,
		HasUID:         true,
		SID:            childInfo.SID,
		Sess:           childInfo.Sess,
		VerifiedAt:     time.Now().Add(-10 * time.Second),
		ExitObservedAt: time.Now().Add(-5 * time.Second),
		KnownMembers: map[int]string{
			leaderPID: leaderTok,
			childPID:  childInfo.StartToken,
		},
	}

	if err := stopAdoptedProcessGroupAfterExitTrack(&proof); err != nil {
		t.Fatalf("stopAdoptedProcessGroupAfterExitTrack err = %v, want nil after stopping verified sibling", err)
	}
	for wait := 0; wait < 20 && inspectProcess(childPID).Alive; wait++ {
		time.Sleep(25 * time.Millisecond)
	}
	if inspectProcess(childPID).Alive {
		t.Fatalf("verified child pid %d survived cleanup past late unverified member", childPID)
	}
	if !inspectProcess(latePID).Alive {
		t.Fatalf("late unverified pid %d was killed unexpectedly", latePID)
	}
}
