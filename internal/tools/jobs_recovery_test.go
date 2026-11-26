package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/checkpoint"
)

type crashHelperMeta struct {
	JobID          string `json:"job_id"`
	Started        string `json:"started"`
	OutputFile     string `json:"output_file"`
	PID            int    `json:"pid"`
	PGID           int    `json:"pgid"`
	ProcessStarted string `json:"process_started"`
}

// TestCrashRecoveryHelperProcess is invoked in a subprocess to start real
// background work and wait indefinitely until the parent kills it with SIGKILL.
func TestCrashRecoveryHelperProcess(t *testing.T) {
	if os.Getenv("GMH_TOOLS_CRASH_HELPER") != "1" {
		return
	}
	root := os.Getenv("GMH_HELPER_ROOT")
	cwd := os.Getenv("GMH_HELPER_CWD")
	outDir := os.Getenv("GMH_HELPER_OUTDIR")
	jobsDir := os.Getenv("GMH_HELPER_JOBSDIR")
	outputID := os.Getenv("GMH_HELPER_OUTID")
	command := os.Getenv("GMH_HELPER_CMD")
	waitMarker := os.Getenv("GMH_HELPER_WAIT_TEXT")
	foreground := os.Getenv("GMH_HELPER_FOREGROUND") == "1"

	e := &Executor{
		Root:      root,
		OutputDir: outDir,
		OutputID:  outputID,
		JobsDir:   jobsDir,
	}
	var (
		job *Job
		err error
	)
	if foreground {
		job, err = e.BashForeground(context.Background(), BashArgs{
			Command: command,
			Cwd:     cwd,
		})
	} else {
		job, err = e.BashBackground(context.Background(), BashArgs{
			Command:    command,
			Cwd:        cwd,
			Background: true,
		})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper Bash start failed: %v\n", err)
		os.Exit(2)
	}
	if waitMarker != "" && job.OutputFile != "" {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(job.OutputFile)
			if err == nil && strings.Contains(string(data), waitMarker) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	snap := job.Snapshot()
	meta := crashHelperMeta{
		JobID:          snap.ID,
		Started:        snap.Started.UTC().Format(time.RFC3339Nano),
		OutputFile:     snap.OutputFile,
		PID:            snap.Identity.PID,
		PGID:           snap.Identity.PGID,
		ProcessStarted: snap.Identity.StartToken,
	}
	enc, _ := json.Marshal(meta)
	fmt.Println(string(enc))
	select {}
}

func spawnCrashHelper(t *testing.T, env map[string]string) (*exec.Cmd, crashHelperMeta) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestCrashRecoveryHelperProcess$")
	cmd.Env = append(os.Environ(), "GMH_TOOLS_CRASH_HELPER=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("crash helper produced no metadata line: %v", scanner.Err())
	}
	var meta crashHelperMeta
	if err := json.Unmarshal([]byte(scanner.Text()), &meta); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("decode crash helper metadata: %v", err)
	}
	t.Cleanup(func() {
		if meta.PID > 0 {
			if p, err := os.FindProcess(meta.PID); err == nil {
				_ = p.Kill()
			}
		}
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, meta
}

func TestCrashRecoveryAdoptsLiveProcess(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	sessionBase := t.TempDir()
	outID := "sess-live-1"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	releaseFile := filepath.Join(workDir, "release")
	doneFile := filepath.Join(workDir, "finished.txt")

	helperCmd, meta := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_CWD":       "work",
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "BEFORE-CRASH",
		"GMH_HELPER_CMD": fmt.Sprintf(
			"printf 'BEFORE-CRASH\\n'; while [ ! -f %q ]; do sleep 0.05; done; printf 'AFTER-CRASH\\n' > %q; exit 7",
			releaseFile, doneFile,
		),
	})

	if meta.PID <= 0 || meta.ProcessStarted == "" {
		t.Fatalf("expected valid process identity before crash, got %+v", meta)
	}
	// Simulate sudden harness crash (SIGKILL): no CloseJobs or normal exit runs.
	if err := helperCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helperCmd.Wait()

	if _, tok, alive := ProcessIdentity(meta.PID); !alive || tok != meta.ProcessStarted {
		t.Fatalf("background process did not survive harness crash cleanly: alive=%t tok=%q want=%q", alive, tok, meta.ProcessStarted)
	}

	recoveredExec := &Executor{
		Root:      root,
		OutputDir: outDir,
		OutputID:  outID,
		JobsDir:   jobsDir,
	}
	recoveredExec.SwitchSession(outID)
	defer recoveredExec.CloseJobs(context.Background())

	// Concurrent calls to RecoverSessionJobs must only adopt and monitor once.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	results := make(chan []RecoveredJob, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := recoveredExec.RecoverSessionJobs(outID, nil, nil, nil)
			errs <- err
			results <- r
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent RecoverSessionJobs failed: %v", err)
		}
	}
	var recovered []RecoveredJob
	for r := range results {
		recovered = r
		if len(r) != 1 {
			t.Fatalf("concurrent recovered %d jobs, want 1: %+v", len(r), r)
		}
	}
	rj := recovered[0]
	if rj.Status != JobRunning || !rj.Adopted || rj.PID != meta.PID || rj.Cwd != workDir || rj.OutputFile != meta.OutputFile {
		t.Fatalf("unexpected recovered job metadata: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "pipe closed") {
		t.Fatalf("expected explicit pipe-closure notice in Reason, got %q", rj.Reason)
	}

	// Output file access via Read remains intact after adoption.
	readRes, err := recoveredExec.Dispatch(context.Background(), "read", fmt.Sprintf(`{"path":%q}`, meta.OutputFile))
	if err != nil {
		t.Fatalf("Read recovered OutputFile failed: %v", err)
	}
	if !strings.Contains(readRes.Text, "BEFORE-CRASH") {
		t.Fatalf("recovered output file missing pre-crash text: %q", readRes.Text)
	}

	// Subsequent background jobs advance sequence past recovered IDs without collision.
	second, err := recoveredExec.BashBackground(context.Background(), BashArgs{Command: "printf next-job"})
	if err != nil {
		t.Fatal(err)
	}
	<-second.Done()
	if second.ID == meta.JobID || second.ID != "bg-2" {
		t.Fatalf("second job ID = %q, want bg-2 (must not collide with %q)", second.ID, meta.JobID)
	}

	// Release the adopted background job and observe real completion.
	if err := os.WriteFile(releaseFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	adoptedJob := recoveredExec.Jobs()[0]
	select {
	case <-adoptedJob.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for adopted job exit")
	}
	if adoptedJob.Status() != JobDone {
		t.Fatalf("adopted job status = %q, want done", adoptedJob.Status())
	}
	res, ok := adoptedJob.Result()
	if !ok {
		t.Fatal("adopted job Result ok = false after completion")
	}
	if !strings.Contains(res.Output, "pipe closed") {
		t.Fatalf("adopted completion output missing pipe closure notice: %q", res.Output)
	}
	if runtime.GOOS == "darwin" {
		if res.ExitCodeUnknown || res.ExitCode != 7 {
			t.Fatalf("darwin adopted job result = %+v, want exit 7", res)
		}
	}
	marker, err := os.ReadFile(doneFile)
	if err != nil || !strings.Contains(string(marker), "AFTER-CRASH") {
		t.Fatalf("adopted job did not finish post-crash work cleanly: err=%v data=%q", err, marker)
	}
}

func TestCrashRecoveryForegroundBeforeDetachAndForgetCleanup(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-fg-1"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	releaseFile := filepath.Join(root, "release-fg")

	helperCmd, meta := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":       root,
		"GMH_HELPER_OUTDIR":     outDir,
		"GMH_HELPER_JOBSDIR":    jobsDir,
		"GMH_HELPER_OUTID":      outID,
		"GMH_HELPER_FOREGROUND": "1",
		"GMH_HELPER_WAIT_TEXT":  "FG-BEFORE",
		"GMH_HELPER_CMD":        fmt.Sprintf("printf 'FG-BEFORE\\n'; while [ ! -f %q ]; do sleep 0.05; done; exit 0", releaseFile),
	})
	if err := helperCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helperCmd.Wait()

	recoveredExec := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	recoveredExec.SwitchSession(outID)
	defer recoveredExec.CloseJobs(context.Background())

	recovered, err := recoveredExec.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Status != JobRunning || !recovered[0].Adopted {
		t.Fatalf("expected interrupted foreground job to be adopted on reopen, got %+v", recovered)
	}

	// Also verify CloseJobs synchronously signals the adopted process group before returning.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := recoveredExec.CloseJobs(ctx); err != nil {
		t.Fatalf("CloseJobs on adopted job failed: %v", err)
	}
	if _, _, alive := ProcessIdentity(meta.PID); alive {
		t.Fatalf("adopted process %d still alive after CloseJobs returned", meta.PID)
	}

	// Verify ForgetJob removes transient foreground sidecars completely.
	e2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: t.TempDir()}
	fgJob, err := e2.BashForeground(context.Background(), BashArgs{Command: "printf fast"})
	if err != nil {
		t.Fatal(err)
	}
	<-fgJob.Done()
	e2.ForgetJob(fgJob)
	left, err := loadPersistedJobs(e2.JobsDir)
	if err != nil || len(left) != 0 {
		t.Fatalf("ForgetJob left persisted records: err=%v left=%+v", err, left)
	}
}

func TestCrashRecoveryDeadProcessBecomesUnknown(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-dead-1"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")

	helperCmd, meta := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "DEAD-PARTIAL",
		"GMH_HELPER_CMD":       "printf 'DEAD-PARTIAL\\n'; sleep 0.25; exit 3",
	})
	if err := helperCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helperCmd.Wait()

	// Wait until the exited background process is fully gone from the process table.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, alive := ProcessIdentity(meta.PID); !alive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	recoveredExec := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	recoveredExec.SwitchSession(outID)
	defer recoveredExec.CloseJobs(context.Background())

	startedAt, err := time.Parse(time.RFC3339Nano, meta.Started)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoveredExec.RecoverSessionJobs(outID, []checkpoint.Job{{
		ID:             meta.JobID,
		Started:        startedAt,
		Command:        "printf 'DEAD-PARTIAL\\n'; sleep 0.25; exit 3",
		Cwd:            root,
		OutputFile:     meta.OutputFile,
		OutputID:       outID,
		PID:            meta.PID,
		PGID:           meta.PGID,
		ProcessStarted: meta.ProcessStarted,
		Status:         checkpoint.Running,
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Status != JobUnknown || recovered[0].Adopted {
		t.Fatalf("expected single unknown non-adopted job, got %+v", recovered)
	}
	// The job is already in this registry. Another recovery must report that
	// same object and must not create a second writer for its record.
	first := recoveredExec.Jobs()[0]
	again, err := recoveredExec.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != recovered[0].ID || again[0].Status != JobUnknown {
		t.Fatalf("second recovery = %+v, want the same unknown job", again)
	}
	jobs := recoveredExec.Jobs()
	if len(jobs) != 1 || jobs[0] != first {
		t.Fatalf("second recovery replaced the live job: %d jobs, same=%v", len(jobs), len(jobs) == 1 && jobs[0] == first)
	}

	job := jobs[0]
	if job.Status() != JobUnknown {
		t.Fatalf("job.Status() = %q, want unknown", job.Status())
	}
	if _, err := recoveredExec.StopJob(job.ID); err == nil {
		t.Fatal("StopJob on unknown job should refuse explicitly")
	}
	readRes, err := recoveredExec.Dispatch(context.Background(), "read", fmt.Sprintf(`{"path":%q}`, meta.OutputFile))
	if err != nil || !strings.Contains(readRes.Text, "DEAD-PARTIAL") {
		t.Fatalf("expected readable partial output for unknown job, err=%v text=%q", err, readRes.Text)
	}
}

func TestCrashRecoveryRefusesReusedPID(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-reuse-1"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(outDir, "bash-reused.log")
	if err := os.WriteFile(outFile, []byte("old-output\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Start an unrelated live process whose PID collides with a persisted job.
	unrelated := exec.CommandContext(t.Context(), "sleep", "30")
	configureProcess(unrelated)
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if unrelated.Process != nil {
			_ = stopBashProcessGroup(unrelated)
			_ = unrelated.Wait()
		}
	})
	unrelatedPID := unrelated.Process.Pid
	pgid, realToken, alive := ProcessIdentity(unrelatedPID)
	if !alive || realToken == "" {
		t.Fatalf("unrelated process %d not inspectable: pgid=%d tok=%q alive=%t", unrelatedPID, pgid, realToken, alive)
	}

	oldStartToken := realToken + "-previous-incarnation"
	if err := savePersistedJob(jobsDir, PersistedJob{
		ID:             "bg-1",
		Started:        time.Now().Add(-time.Minute),
		Command:        "touch side-effect",
		Cwd:            root,
		OutputFile:     outFile,
		OutputID:       outID,
		PID:            unrelatedPID,
		PGID:           pgid,
		ProcessStarted: oldStartToken,
		Status:         JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatalf("RecoverSessionJobs failed: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Status != JobUnknown || recovered[0].Adopted {
		t.Fatalf("expected PID reuse rejection -> unknown, got %+v", recovered)
	}
	if !strings.Contains(strings.ToLower(recovered[0].Reason), "mismatch") {
		t.Fatalf("expected mismatch explanation in reason, got %q", recovered[0].Reason)
	}

	job := exec2.Jobs()[0]
	job.Cancel()
	if _, err := exec2.StopJob("bg-1"); err == nil {
		t.Fatal("StopJob should refuse reused-PID unknown job")
	}
	if err := exec2.CloseJobs(context.Background()); err != nil {
		t.Fatalf("CloseJobs failed on unknown job: %v", err)
	}

	// Critical safety invariant: the unrelated process MUST STILL BE ALIVE.
	_, afterToken, stillAlive := ProcessIdentity(unrelatedPID)
	if !stillAlive || afterToken != realToken {
		t.Fatalf("unrelated process PID %d was killed or altered! alive=%t tok=%q want=%q", unrelatedPID, stillAlive, afterToken, realToken)
	}
}

func TestCrashRecoveryFailClosedSecurity(t *testing.T) {
	t.Run("rejects symlink in jobs dir", func(t *testing.T) {
		root := t.TempDir()
		sessionBase := t.TempDir()
		outID := "sess-sym"
		outDir := filepath.Join(sessionBase, outID, "bash-output")
		jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
		if err := os.MkdirAll(jobsDir, 0700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(jobsDir, "bg-1-0001.json")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		exec := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
		if _, err := exec.RecoverSessionJobs(outID, nil, nil, nil); err == nil {
			t.Fatal("expected symlink inside JobsDir to fail closed")
		}
	})

	t.Run("strips foreign output file path", func(t *testing.T) {
		root := t.TempDir()
		sessionBase := t.TempDir()
		outID := "sess-foreign"
		outDir := filepath.Join(sessionBase, outID, "bash-output")
		jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
		secretFile := filepath.Join(t.TempDir(), "bash-secret.log")
		if err := os.WriteFile(secretFile, []byte("SECRET"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := savePersistedJob(jobsDir, PersistedJob{
			ID:         "bg-1",
			Started:    time.Now(),
			Command:    "echo hi",
			Cwd:        root,
			OutputFile: secretFile,
			OutputID:   outID,
			Status:     JobDone,
		}); err != nil {
			t.Fatal(err)
		}
		exec := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
		exec.SwitchSession(outID)
		recovered, err := exec.RecoverSessionJobs(outID, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(recovered) != 1 || recovered[0].OutputFile != "" {
			t.Fatalf("expected foreign OutputFile to be stripped, got %+v", recovered)
		}
		if _, err := exec.Dispatch(context.Background(), "read", fmt.Sprintf(`{"path":%q}`, secretFile)); err == nil {
			t.Fatal("Read must not gain access to foreign OutputFile")
		}
	})

	t.Run("empty OutputDir fails closed on output paths", func(t *testing.T) {
		secretDir := t.TempDir()
		secretFile := filepath.Join(secretDir, "bash-secret.log")
		if err := os.WriteFile(secretFile, []byte("TOP-SECRET"), 0600); err != nil {
			t.Fatal(err)
		}
		if validRecoveredOutputPath(secretFile, "", "", "") {
			t.Fatal("validRecoveredOutputPath must return false when exec OutputDir is empty")
		}
		exec := &Executor{Root: t.TempDir(), JobsDir: t.TempDir()}
		exec.SwitchSession("s-no-out")
		_ = savePersistedJob(exec.JobsDir, PersistedJob{
			ID:         "bg-1",
			Started:    time.Now(),
			Command:    "echo hi",
			Cwd:        exec.Root,
			OutputFile: secretFile,
			Status:     JobDone,
			Error:      "failed without code",
		})
		recovered, err := exec.RecoverSessionJobs("s-no-out", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(recovered) != 1 || recovered[0].OutputFile != "" {
			t.Fatalf("expected empty OutputFile when exec OutputDir unset, got %+v", recovered)
		}
		// Done without ExitCode must report ExitCodeUnknown rather than 0.
		snap := exec.Jobs()[0].Snapshot()
		if !snap.Result.ExitCodeUnknown {
			t.Fatalf("expected ExitCodeUnknown=true when ExitCode is nil, got %+v", snap.Result)
		}
	})
}

func spawnLiveSleepGroup(t *testing.T, cmdStr string) (int, int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", "-c", cmdStr)
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	pgid := processGroupID(pid)
	_, startToken, alive := ProcessIdentity(pid)
	if !alive || startToken == "" {
		_ = killProcessTree(cmd)
		_ = cmd.Wait()
		t.Fatalf("spawned process %d not inspectable", pid)
	}
	t.Cleanup(func() {
		_ = killProcessTree(cmd)
		_ = cmd.Wait()
	})
	return pid, pgid, startToken
}

func TestCrashRecoveryAdoptedShortDeadlineEnforced(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-deadline-short"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(outDir, "bash-1.log")
	if err := os.WriteFile(outFile, []byte("started\n"), 0600); err != nil {
		t.Fatal(err)
	}

	childPIDFile := filepath.Join(root, "child.pid")
	cmd := exec.CommandContext(t.Context(), "bash", "-c", fmt.Sprintf("sleep 30 & echo $! > %q; wait", childPIDFile))
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = killProcessTree(cmd)
	})
	pid := cmd.Process.Pid
	pgid := processGroupID(pid)
	_, startToken, alive := ProcessIdentity(pid)
	if !alive || startToken == "" {
		t.Fatalf("process %d not inspectable", pid)
	}
	var childPID int
	waitChild := time.Now().Add(3 * time.Second)
	for time.Now().Before(waitChild) {
		if data, err := os.ReadFile(childPIDFile); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && p > 0 {
				childPID = p
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("background child pid not written")
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})

	started := time.Now().Add(-500 * time.Millisecond)
	deadline := time.Now().Add(500 * time.Millisecond)
	if err := savePersistedJob(jobsDir, PersistedJob{
		ID:             "bg-1",
		Started:        started,
		Deadline:       deadline,
		Command:        "sleep 30",
		Cwd:            root,
		OutputFile:     outFile,
		OutputID:       outID,
		PID:            pid,
		PGID:           pgid,
		ProcessStarted: startToken,
		Status:         JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Status != JobRunning || !recovered[0].Adopted {
		t.Fatalf("expected adopted running job, got %+v", recovered)
	}
	job := exec2.Jobs()[0]
	if !job.Snapshot().Deadline.Equal(deadline) {
		t.Fatalf("adopted deadline = %v, want %v", job.Snapshot().Deadline, deadline)
	}

	// Simulate a second crash check: the persisted record must retain the exact deadline.
	persisted, err := loadPersistedJobs(jobsDir)
	if err != nil || len(persisted) != 1 || !persisted[0].Deadline.Equal(deadline) {
		t.Fatalf("persisted deadline after adoption = %+v, err=%v, want %v", persisted, err, deadline)
	}

	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("adopted job was not stopped when persisted deadline expired")
	}
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed-out adopted process did not exit")
	}
	res, ok := job.Result()
	if !ok || !IsTimeout(res.Err) {
		t.Fatalf("adopted deadline result = %+v (ok=%t), want timeoutError", res, ok)
	}
	pollDead := time.Now().Add(3 * time.Second)
	for time.Now().Before(pollDead) {
		if _, _, childAlive := ProcessIdentity(childPID); !childAlive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, childAlive := ProcessIdentity(childPID); childAlive {
		t.Fatalf("child pid %d survived adopted job deadline timeout", childPID)
	}
}

func TestCrashRecoveryExpiredDeadlineStoppedOnAdoption(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-deadline-expired"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(outDir, "bash-1.log")
	if err := os.WriteFile(outFile, []byte("ran before crash\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", "-c", "sleep 30")
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() { _ = killProcessTree(cmd) })

	pid := cmd.Process.Pid
	pgid := processGroupID(pid)
	_, startToken, alive := ProcessIdentity(pid)
	if !alive || startToken == "" {
		t.Fatalf("process %d not inspectable", pid)
	}

	// Also spawn an explicitly unlimited job that started >2h ago; it must stay running.
	unlimCmd := exec.CommandContext(t.Context(), "bash", "-c", "sleep 30")
	configureProcess(unlimCmd)
	if err := unlimCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = killProcessTree(unlimCmd)
		_ = unlimCmd.Wait()
	})
	unlimPID := unlimCmd.Process.Pid
	unlimPGID := processGroupID(unlimPID)
	_, unlimToken, unlimAlive := ProcessIdentity(unlimPID)
	if !unlimAlive || unlimToken == "" {
		t.Fatalf("unlimited process %d not inspectable", unlimPID)
	}

	if err := savePersistedJob(jobsDir, PersistedJob{
		ID:             "bg-1",
		Started:        time.Now().Add(-5 * time.Minute),
		Deadline:       time.Now().Add(-1 * time.Minute),
		Command:        "sleep 30",
		Cwd:            root,
		OutputFile:     outFile,
		OutputID:       outID,
		PID:            pid,
		PGID:           pgid,
		ProcessStarted: startToken,
		Status:         JobRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := savePersistedJob(jobsDir, PersistedJob{
		ID:             "bg-2",
		Started:        time.Now().Add(-3 * time.Hour),
		Unlimited:      true,
		Command:        "sleep 30",
		Cwd:            root,
		OutputID:       outID,
		PID:            unlimPID,
		PGID:           unlimPGID,
		ProcessStarted: unlimToken,
		Status:         JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 2 {
		t.Fatalf("recovered len = %d, want 2: %+v", len(recovered), recovered)
	}
	var expiredRJ, unlimRJ RecoveredJob
	for _, rj := range recovered {
		if rj.ID == "bg-1" {
			expiredRJ = rj
		} else if rj.ID == "bg-2" {
			unlimRJ = rj
		}
	}
	if expiredRJ.Status != JobDone || !expiredRJ.Adopted || !strings.Contains(expiredRJ.Reason, "exceeded deadline") {
		t.Fatalf("expired job not stopped on adoption with clear recovery note: %+v", expiredRJ)
	}
	if unlimRJ.Status != JobRunning || !unlimRJ.Adopted {
		t.Fatalf("unlimited job should stay running on adoption: %+v", unlimRJ)
	}
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("expired process was not killed during RecoverSessionJobs")
	}
	if _, _, stillAlive := ProcessIdentity(pid); stillAlive {
		t.Fatalf("expired PID %d still alive after adoption", pid)
	}
	res, ok := exec2.Jobs()[0].Result()
	if expiredJob := exec2.registry().Get("bg-1"); expiredJob == nil {
		t.Fatal("missing bg-1")
	} else {
		res, ok = expiredJob.Result()
		if !ok || !IsTimeout(res.Err) || !strings.Contains(res.Output, "exceeded deadline") {
			t.Fatalf("expired job result = %+v (ok=%t), want timeoutError + recovery note", res, ok)
		}
	}
}

func TestCrashRecoveryLegacyRecordWithoutDeadlineUsesDefaultLifetime(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-legacy-deadline"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")

	// Case 1: recent legacy record without Deadline -> Deadline = Started + DefaultBackgroundMaxLifetime.
	cmd1 := exec.CommandContext(t.Context(), "bash", "-c", "sleep 30")
	configureProcess(cmd1)
	if err := cmd1.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = killProcessTree(cmd1)
		_ = cmd1.Wait()
	})
	pid1 := cmd1.Process.Pid
	pgid1 := processGroupID(pid1)
	_, tok1, alive1 := ProcessIdentity(pid1)
	if !alive1 {
		t.Fatalf("process %d not alive", pid1)
	}
	started1 := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)

	// Case 2: old legacy record (> DefaultBackgroundMaxLifetime ago) -> stopped on adoption.
	cmd2 := exec.CommandContext(t.Context(), "bash", "-c", "sleep 30")
	configureProcess(cmd2)
	if err := cmd2.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone2 := make(chan error, 1)
	go func() { waitDone2 <- cmd2.Wait() }()
	t.Cleanup(func() { _ = killProcessTree(cmd2) })
	pid2 := cmd2.Process.Pid
	pgid2 := processGroupID(pid2)
	_, tok2, alive2 := ProcessIdentity(pid2)
	if !alive2 {
		t.Fatalf("process %d not alive", pid2)
	}
	started2 := time.Now().Add(-(DefaultBackgroundMaxLifetime + time.Minute)).UTC().Truncate(time.Second)

	if err := savePersistedJob(jobsDir, PersistedJob{
		ID:             "bg-1",
		Started:        started1,
		Command:        "sleep 30",
		Cwd:            root,
		OutputID:       outID,
		PID:            pid1,
		PGID:           pgid1,
		ProcessStarted: tok1,
		Status:         JobRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := savePersistedJob(jobsDir, PersistedJob{
		ID:             "bg-2",
		Started:        started2,
		Command:        "sleep 30",
		Cwd:            root,
		OutputID:       outID,
		PID:            pid2,
		PGID:           pgid2,
		ProcessStarted: tok2,
		Status:         JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 2 {
		t.Fatalf("recovered len = %d, want 2", len(recovered))
	}
	j1 := exec2.registry().Get("bg-1")
	if j1 == nil || j1.Status() != JobRunning {
		t.Fatalf("recent legacy job = %v, want running", j1)
	}
	wantDeadline := started1.Add(DefaultBackgroundMaxLifetime)
	if !j1.Snapshot().Deadline.Equal(wantDeadline) {
		t.Fatalf("legacy job deadline = %v, want %v", j1.Snapshot().Deadline, wantDeadline)
	}
	j2 := exec2.registry().Get("bg-2")
	if j2 == nil || j2.Status() != JobDone {
		t.Fatalf("expired legacy job status = %v, want done", j2)
	}
	select {
	case <-waitDone2:
	case <-time.After(3 * time.Second):
		t.Fatal("expired legacy job process was not stopped on adoption")
	}
}

func TestCrashRecoveryWaitErrorKeepsJobRunningAndStoppable(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-wait-err"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(outDir, "bash-wait.log")
	if err := os.WriteFile(outFile, []byte("wait-err-log\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", "-c", "sleep 30")
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() { _ = killProcessTree(cmd) })

	pid := cmd.Process.Pid
	pgid := processGroupID(pid)
	_, startToken, alive := ProcessIdentity(pid)
	if !alive {
		t.Fatalf("process %d not alive", pid)
	}
	if err := savePersistedJob(jobsDir, PersistedJob{
		ID:             "bg-1",
		Started:        time.Now().Add(-time.Second),
		Deadline:       time.Now().Add(time.Minute),
		Command:        "sleep 30",
		Cwd:            root,
		OutputFile:     outFile,
		OutputID:       outID,
		PID:            pid,
		PGID:           pgid,
		ProcessStarted: startToken,
		Status:         JobRunning,
	}); err != nil {
		t.Fatal(err)
	}

	waitCalled := make(chan struct{})
	exec2 := &Executor{
		Root:      root,
		OutputDir: outDir,
		OutputID:  outID,
		JobsDir:   jobsDir,
		waitAdopted: func(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
			close(waitCalled)
			return -1, false, errors.New("injected kevent EACCES")
		},
	}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Status != JobRunning {
		t.Fatalf("recovered = %+v, want running", recovered)
	}
	<-waitCalled

	job := exec2.registry().Get("bg-1")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if job.Snapshot().WaitError != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	snap := job.Snapshot()
	if snap.Status != JobRunning {
		t.Fatalf("job status after wait error = %q, want running (must never mark done)", snap.Status)
	}
	if !strings.Contains(snap.WaitError, "injected kevent EACCES") || !strings.Contains(snap.RecoveryNote, "injected kevent EACCES") {
		t.Fatalf("wait error not reported in snapshot: %+v", snap)
	}
	select {
	case <-job.Done():
		t.Fatal("job.Done() closed after wait error")
	default:
	}

	// Active output mark must remain while the job is still running.
	absOutDir, _ := filepath.Abs(outDir)
	exec2.outMu.Lock()
	budget := exec2.outputBudgets[absOutDir]
	exec2.outMu.Unlock()
	if budget == nil {
		t.Fatal("expected active output budget entry")
	}
	budget.mu.Lock()
	activeStillMarked := budget.active[filepath.Base(outFile)]
	budget.mu.Unlock()
	if !activeStillMarked {
		t.Fatal("active output mark was removed despite wait error")
	}

	// Process must still be alive, and StopJob must succeed and kill it cleanly.
	if _, _, stillAlive := ProcessIdentity(pid); !stillAlive {
		t.Fatalf("process %d exited unexpectedly", pid)
	}
	stopRes, stopErr := exec2.StopJob("bg-1")
	if stopErr != nil {
		t.Fatalf("StopJob after wait error failed: %v (%s)", stopErr, stopRes.Text)
	}
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("process was not killed by StopJob after wait error")
	}
	if job.Status() != JobDone {
		t.Fatalf("job status after StopJob = %q, want done", job.Status())
	}
	budget.mu.Lock()
	activeAfterStop := budget.active[filepath.Base(outFile)]
	budget.mu.Unlock()
	if activeAfterStop {
		t.Fatal("active output mark was not cleared after StopJob")
	}
}

func TestCrashRecoveryAdoptedLeaderExitStopsBackgroundChild(t *testing.T) {
	root := t.TempDir()
	sessionBase := t.TempDir()
	outID := "sess-child-cleanup"
	outDir := filepath.Join(sessionBase, outID, "bash-output")
	jobsDir := filepath.Join(sessionBase, outID, "bash-output-jobs")
	childPIDFile := filepath.Join(root, "child.pid")
	releaseFile := filepath.Join(root, "release")

	helperCmd, meta := spawnCrashHelper(t, map[string]string{
		"GMH_HELPER_ROOT":      root,
		"GMH_HELPER_OUTDIR":    outDir,
		"GMH_HELPER_JOBSDIR":   jobsDir,
		"GMH_HELPER_OUTID":     outID,
		"GMH_HELPER_WAIT_TEXT": "CHILD-READY",
		"GMH_HELPER_CMD": fmt.Sprintf(
			"sleep 30 & echo $! > %q; printf 'CHILD-READY\\n'; while [ ! -f %q ]; do sleep 0.05; done; exit 0",
			childPIDFile, releaseFile,
		),
	})
	if err := helperCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helperCmd.Wait()

	pidBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child pid %q: %v", string(pidBytes), err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})
	if _, _, childAlive := ProcessIdentity(childPID); !childAlive {
		t.Fatalf("child pid %d not alive before adoption", childPID)
	}

	exec2 := &Executor{Root: root, OutputDir: outDir, OutputID: outID, JobsDir: jobsDir}
	exec2.SwitchSession(outID)
	defer exec2.CloseJobs(context.Background())

	recovered, err := exec2.RecoverSessionJobs(outID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Status != JobRunning {
		t.Fatalf("recovered = %+v, want 1 running", recovered)
	}

	// Let the adopted leader exit cleanly.
	if err := os.WriteFile(releaseFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	job := exec2.registry().Get(meta.JobID)
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("adopted job did not finish after leader exit")
	}

	// The background child spawned by the adopted leader must be stopped.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, childAlive := ProcessIdentity(childPID); !childAlive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background child pid %d stayed alive after adopted leader %d exited", childPID, meta.PID)
}

var _ = errors.New
