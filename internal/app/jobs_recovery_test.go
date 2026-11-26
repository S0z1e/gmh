package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

type appCrashMeta struct {
	SessionID  string `json:"session_id"`
	LiveJobID  string `json:"live_job_id"`
	LivePID    int    `json:"live_pid"`
	DoneJobID  string `json:"done_job_id"`
	DeadJobID  string `json:"dead_job_id"`
	DeadPID    int    `json:"dead_pid"`
	LiveOutput string `json:"live_output"`
	DeadOutput string `json:"dead_output"`
}

func TestAppCrashRecoveryHelperProcess(t *testing.T) {
	if os.Getenv("GMH_APP_CRASH_HELPER") != "1" {
		return
	}
	root := os.Getenv("GMH_HELPER_ROOT")
	releaseFile := os.Getenv("GMH_HELPER_RELEASE")

	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Setup failed: %v\n", err)
		os.Exit(2)
	}
	liveCmd := fmt.Sprintf("printf 'APP-LIVE\\n'; while [ ! -f %q ]; do sleep 0.05; done; exit 0", releaseFile)
	liveJob, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{
		Command:    liveCmd,
		Background: true,
	})
	if err != nil {
		os.Exit(2)
	}
	doneJob, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{
		Command:    "printf 'APP-DONE\\n'; exit 0",
		Background: true,
	})
	if err != nil {
		os.Exit(2)
	}
	// Third real subprocess: alive during helper kill, then exits ~250ms later while down.
	deadJob, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{
		Command:    "printf 'APP-UNKNOWN-LOG\\n'; sleep 0.25; exit 2",
		Background: true,
	})
	if err != nil {
		os.Exit(2)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data1, _ := os.ReadFile(liveJob.OutputFile)
		data3, _ := os.ReadFile(deadJob.OutputFile)
		if strings.Contains(string(data1), "APP-LIVE") && strings.Contains(string(data3), "APP-UNKNOWN-LOG") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-doneJob.Done()
	_ = a.Store.Append(llm.Message{Role: "user", Content: "prior prompt"})

	meta := appCrashMeta{
		SessionID:  a.Store.ID,
		LiveJobID:  liveJob.ID,
		LivePID:    liveJob.PID,
		DoneJobID:  doneJob.ID,
		DeadJobID:  deadJob.ID,
		DeadPID:    deadJob.PID,
		LiveOutput: liveJob.OutputFile,
		DeadOutput: deadJob.OutputFile,
	}
	line, _ := json.Marshal(meta)
	fmt.Println(string(line))
	select {}
}

func TestAppResumeRecoversJobsAndSupportsStopAndLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	releaseFile := filepath.Join(root, "release")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestAppCrashRecoveryHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GMH_APP_CRASH_HELPER=1",
		"GMH_HELPER_ROOT="+root,
		"GMH_HELPER_RELEASE="+releaseFile,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("app crash helper exited early: %v", sc.Err())
	}
	var meta appCrashMeta
	if err := json.Unmarshal([]byte(sc.Text()), &meta); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("decode helper meta: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("go\n"), 0644)
		if meta.LivePID > 0 {
			if p, err := os.FindProcess(meta.LivePID); err == nil {
				_ = p.Kill()
			}
		}
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	// Wait until the short-lived third background job has genuinely exited.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, alive := tools.ProcessIdentity(meta.DeadPID); !alive {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto", Resume: meta.SessionID})
	if err != nil {
		t.Fatalf("Setup(Resume) failed: %v", err)
	}
	defer a.Close()

	if len(a.RecoveredJobs) != 3 {
		t.Fatalf("RecoveredJobs len = %d, want 3: %+v", len(a.RecoveredJobs), a.RecoveredJobs)
	}
	var liveFound, doneFound, unknownFound bool
	for _, rj := range a.RecoveredJobs {
		switch rj.ID {
		case meta.LiveJobID:
			liveFound = rj.Status == tools.JobRunning && rj.Adopted && rj.PID == meta.LivePID
		case meta.DoneJobID:
			doneFound = rj.Status == tools.JobDone
		case meta.DeadJobID:
			unknownFound = rj.Status == tools.JobUnknown
		}
	}
	if !liveFound || !doneFound || !unknownFound {
		t.Fatalf("unexpected RecoveredJobs: %+v", a.RecoveredJobs)
	}
	if !strings.Contains(a.ResumedMessage, "1 running") ||
		!strings.Contains(a.ResumedMessage, "1 unknown") ||
		!strings.Contains(a.ResumedMessage, "1 finished") ||
		!strings.Contains(a.ResumedMessage, "(adopted)") {
		t.Fatalf("ResumedMessage missing clear recovery summary: %q", a.ResumedMessage)
	}

	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	if quit := r.Handle(context.Background(), "/jobs"); quit {
		t.Fatal("/jobs quit")
	}
	joined := strings.Join(printed, "\n")
	if !strings.Contains(joined, "(adopted)") || !strings.Contains(joined, "unknown") {
		t.Fatalf("/jobs missing adopted/unknown indicators: %v", printed)
	}

	// Bare "/jobs stop" and "/jobs kill" show usage instead of falling through as prompts.
	for _, bare := range []string{"/jobs stop", "/jobs kill"} {
		printed = nil
		r.Handle(context.Background(), bare)
		if !strings.Contains(strings.Join(printed, "\n"), "Usage: /jobs stop") {
			t.Fatalf("%q should print usage, got %v", bare, printed)
		}
	}

	// Stopping unknown job refuses without signaling and keeps History in sync if settled.
	printed = nil
	r.Handle(context.Background(), "/jobs stop "+meta.DeadJobID)
	if !strings.Contains(strings.Join(printed, "\n"), "[error]") {
		t.Fatalf("/jobs stop on unknown should print error: %v", printed)
	}
	histAfterUnknownStop := len(a.History)

	// Explicitly stop the adopted live process via "/jobs kill bg-1 " (trailing space!).
	printed = nil
	r.Handle(context.Background(), "/jobs kill "+meta.LiveJobID+" ")
	if !strings.Contains(strings.Join(printed, "\n"), "Stopped background job "+meta.LiveJobID) &&
		!strings.Contains(strings.Join(printed, "\n"), "Signalled background job "+meta.LiveJobID) {
		t.Fatalf("/jobs kill with trailing space unexpected output: %v", printed)
	}
	if len(a.History) <= histAfterUnknownStop {
		t.Fatalf("expected /jobs stop to synchronize newly saved job messages into App.History")
	}

	// Verify RenameSession preserves output/jobs ownership and DeleteSession removes it.
	outputID := a.Store.OutputID()
	if err := a.RenameSession(meta.SessionID, "renamed-recovery"); err != nil {
		t.Fatalf("RenameSession failed: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	jobsDir := filepath.Join(session.Dir(), outputID, "bash-output-jobs")
	if _, err := os.Stat(jobsDir); err != nil {
		t.Fatalf("JobsDir should stay intact after rename under OutputID: %v", err)
	}

	// Test failed LoadSession rolls back state cleanly without corrupting current session.
	a2, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	origStore := a2.Store
	// Poison renamed-recovery's JobsDir with a symlink so LoadSession fails closed.
	_ = os.Symlink(filepath.Join(root, "outside"), filepath.Join(jobsDir, "bad.json"))
	if _, err := a2.LoadSession("renamed-recovery"); err == nil {
		t.Fatal("expected LoadSession to fail closed on symlinked JobsDir entry")
	}
	if a2.Store != origStore || a2.Cfg.Session != origStore {
		t.Fatal("failed LoadSession mutated App.Store / Cfg.Session")
	}

	if err := session.DeleteSession("renamed-recovery"); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(session.Dir(), outputID)); !os.IsNotExist(err) {
		t.Fatalf("DeleteSession did not clean up output family including bash-output-jobs: %v", err)
	}
}

func TestAppSurfacesOrphanedJobsFromOtherWorkspaceSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	otherRoot := t.TempDir()
	writeMockProvider(t, otherRoot)
	releaseFile := filepath.Join(root, "release-orphan")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestAppCrashRecoveryHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GMH_APP_CRASH_HELPER=1",
		"GMH_HELPER_ROOT="+root,
		"GMH_HELPER_RELEASE="+releaseFile,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("app crash helper exited early: %v", sc.Err())
	}
	var meta appCrashMeta
	if err := json.Unmarshal([]byte(sc.Text()), &meta); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("decode helper meta: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("go\n"), 0644)
		if meta.LivePID > 0 {
			if p, err := os.FindProcess(meta.LivePID); err == nil {
				_ = p.Kill()
			}
		}
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	// Wait until the short-lived job exits, while meta.LiveJobID stays alive.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, alive := tools.ProcessIdentity(meta.DeadPID); !alive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Open a brand new session in the SAME workspace without --resume.
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if len(a.OrphanedJobs) != 1 || a.OrphanedJobs[0].SessionID != meta.SessionID || len(a.OrphanedJobs[0].Jobs) != 1 {
		t.Fatalf("OrphanedJobs = %+v, want session %s with 1 live job", a.OrphanedJobs, meta.SessionID)
	}
	if !strings.Contains(a.OrphanedJobsNotice, meta.SessionID) || !strings.Contains(a.OrphanedJobsNotice, "--resume "+meta.SessionID) {
		t.Fatalf("OrphanedJobsNotice = %q, want session id %s", a.OrphanedJobsNotice, meta.SessionID)
	}
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	r.Handle(context.Background(), "/jobs")
	if !strings.Contains(strings.Join(printed, "\n"), meta.SessionID) {
		t.Fatalf("/jobs output missing orphaned session notice: %v", printed)
	}

	// A different workspace must NOT see root's orphaned jobs.
	otherApp, err := Setup(Params{WorkspaceTrust: "trust", Root: otherRoot, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer otherApp.Close()
	if len(otherApp.OrphanedJobs) != 0 || otherApp.OrphanedJobsNotice != "" {
		t.Fatalf("other workspace saw foreign orphaned jobs: %+v %q", otherApp.OrphanedJobs, otherApp.OrphanedJobsNotice)
	}
}

func TestAppBackgroundMaxLifetimeConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	cfgJSON := `{"backgroundMaxLifetimeSeconds":90,"providers":{"mock":{"apiKey":"test","baseUrl":"https://example.invalid","api":"chat","model":"mock"}}}`
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfgJSON), 0600); err != nil {
		t.Fatal(err)
	}
	a1, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Cfg.Exec.BackgroundMaxLifetime != 90*time.Second {
		t.Fatalf("config BackgroundMaxLifetime = %v, want 90s", a1.Cfg.Exec.BackgroundMaxLifetime)
	}
	_ = a1.Close()

	// Env 0 disables the lifetime cap.
	t.Setenv("OPENAI_BACKGROUND_MAX_LIFETIME_SECONDS", "0")
	a2, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if a2.Cfg.Exec.BackgroundMaxLifetime >= 0 {
		t.Fatalf("env 0 BackgroundMaxLifetime = %v, want <0 (unlimited)", a2.Cfg.Exec.BackgroundMaxLifetime)
	}
	_ = a2.Close()

	// Explicit flag/param overrides env.
	a3, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto", BackgroundMaxLifetimeSeconds: 45})
	if err != nil {
		t.Fatal(err)
	}
	if a3.Cfg.Exec.BackgroundMaxLifetime != 45*time.Second {
		t.Fatalf("param BackgroundMaxLifetime = %v, want 45s", a3.Cfg.Exec.BackgroundMaxLifetime)
	}
	_ = a3.Close()
}
