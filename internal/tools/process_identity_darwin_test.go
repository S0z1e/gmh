//go:build darwin

package tools

import (
	"context"
	"errors"
	"path/filepath"
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
