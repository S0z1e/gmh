package rpc

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

	"minimalharness/internal/app"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

type rpcCrashMeta struct {
	SessionID  string `json:"session_id"`
	LiveJobID  string `json:"live_job_id"`
	LivePID    int    `json:"live_pid"`
	LiveOutput string `json:"live_output"`
	DeadJobID  string `json:"dead_job_id"`
	DeadPID    int    `json:"dead_pid"`
	DeadOutput string `json:"dead_output"`
}

func TestRPCCrashRecoveryHelperProcess(t *testing.T) {
	if os.Getenv("GMH_RPC_CRASH_HELPER") != "1" {
		return
	}
	root := os.Getenv("GMH_HELPER_ROOT")
	releaseFile := os.Getenv("GMH_HELPER_RELEASE")

	store, err := session.Open("", session.OpenOptions{Workspace: root})
	if err != nil {
		fmt.Fprintf(os.Stderr, "session.Open failed: %v\n", err)
		os.Exit(2)
	}
	exec := &tools.Executor{
		Root:      root,
		OutputDir: store.OutputDir(),
		OutputID:  store.OutputID(),
		JobsDir:   store.JobsDir(),
	}
	exec.SwitchSession(store.ID)

	liveCmd := fmt.Sprintf("printf 'RPC-LIVE\\n'; while [ ! -f %q ]; do sleep 0.05; done; exit 0", releaseFile)
	liveJob, err := exec.BashBackground(context.Background(), tools.BashArgs{
		Command:    liveCmd,
		Background: true,
	})
	if err != nil {
		os.Exit(2)
	}
	deadJob, err := exec.BashBackground(context.Background(), tools.BashArgs{
		Command:    "printf 'RPC-DEAD\\n'; sleep 0.25; exit 2",
		Background: true,
	})
	if err != nil {
		os.Exit(2)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data1, _ := os.ReadFile(liveJob.OutputFile)
		data2, _ := os.ReadFile(deadJob.OutputFile)
		if strings.Contains(string(data1), "RPC-LIVE") && strings.Contains(string(data2), "RPC-DEAD") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = store.Append(llm.Message{Role: "user", Content: "prior prompt"})

	meta := rpcCrashMeta{
		SessionID:  store.ID,
		LiveJobID:  liveJob.ID,
		LivePID:    liveJob.PID,
		LiveOutput: liveJob.OutputFile,
		DeadJobID:  deadJob.ID,
		DeadPID:    deadJob.PID,
		DeadOutput: deadJob.OutputFile,
	}
	line, _ := json.Marshal(meta)
	fmt.Println(string(line))
	select {}
}

func TestRPCReadyResumeAndStopRecoveredJobs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	config := `{"providers":{"mock":{"apiKey":"test","baseUrl":"https://example.invalid","api":"chat","model":"mock"}}}`
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	releaseFile := filepath.Join(root, "release")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestRPCCrashRecoveryHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GMH_RPC_CRASH_HELPER=1",
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
		t.Fatalf("rpc crash helper exited early: %v", sc.Err())
	}
	var meta rpcCrashMeta
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, alive := tools.ProcessIdentity(meta.DeadPID); !alive {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	a, err := app.Setup(app.Params{WorkspaceTrust: "trust", Root: root, Mode: "auto", Resume: meta.SessionID})
	if err != nil {
		t.Fatalf("Setup(Resume) failed: %v", err)
	}
	defer a.Close()

	d := startDriver(t, a, &app.Runner{App: a, Print: func(string, ...any) {}})
	defer d.quit(t)

	ready := d.waitFor("ready", 2*time.Second)
	if ready["resumed"] != true {
		t.Fatalf("ready.resumed = %v, want true", ready["resumed"])
	}
	if rm, _ := ready["resumed_message"].(string); !strings.Contains(rm, "1 running") || !strings.Contains(rm, "1 unknown") {
		t.Fatalf("ready.resumed_message = %q", rm)
	}

	// Query jobs over RPC and verify status + pid + cwd + output_file + adopted.
	d.send(map[string]any{"id": "jobs-1", "type": "jobs"})
	jobsResp := d.waitFor("response", 2*time.Second)
	dataMap := jobsResp["data"].(map[string]any)
	items := dataMap["jobs"].([]any)
	if len(items) != 2 {
		t.Fatalf("rpc jobs count = %d, want 2", len(items))
	}
	foundUnknownErr := false
	foundLiveAdopted := false
	for _, raw := range items {
		item := raw.(map[string]any)
		switch item["id"] {
		case meta.LiveJobID:
			if item["status"] == "running" && item["adopted"] == true && item["cwd"] == root && item["output_file"] == meta.LiveOutput {
				foundLiveAdopted = true
			}
		case meta.DeadJobID:
			if item["status"] == "unknown" && item["error"] != nil && item["output_file"] == meta.DeadOutput {
				foundUnknownErr = true
			}
		}
	}
	if !foundLiveAdopted || !foundUnknownErr {
		t.Fatalf("rpc jobs missing expected metadata: %+v", items)
	}

	// Bare "/jobs stop" prompt must error cleanly instead of invoking the LLM.
	d.send(map[string]any{"id": "bare-stop", "type": "prompt", "message": "/jobs stop"})
	bareErr := d.waitFor("error", 2*time.Second)
	if bareErr["id"] != "bare-stop" {
		t.Fatalf("expected error for bare /jobs stop prompt, got %v", bareErr)
	}

	// Stopping the unknown job via RPC must fail cleanly without signaling.
	d.send(map[string]any{"id": "stop-unknown", "type": "stop_job", "job_id": meta.DeadJobID})
	errResp := d.waitFor("error", 2*time.Second)
	if errResp["id"] != "stop-unknown" {
		t.Fatalf("expected error response for stopping unknown job, got %v", errResp)
	}

	// Stopping the adopted live job via slash command with trailing space must succeed.
	d.send(map[string]any{"id": "stop-live", "type": "prompt", "message": "/jobs kill " + meta.LiveJobID + " "})
	stopResp := d.waitFor("response", 2*time.Second)
	if stopResp["id"] != "stop-live" {
		t.Fatalf("expected stop-live response, got %v", stopResp)
	}

	// Check state & resume_session from a fresh session back into meta.SessionID.
	d.send(map[string]any{"id": "new-1", "type": "new_session"})
	_ = d.waitFor("response", 2*time.Second)
	d.send(map[string]any{"id": "resume-1", "type": "resume_session", "session": meta.SessionID})
	resumeResp := d.waitFor("response", 2*time.Second)
	rData := resumeResp["data"].(map[string]any)
	if rm, _ := rData["resumed_message"].(string); !strings.Contains(rm, "Recovered 2 background job(s)") {
		t.Fatalf("resume_session missing recovered summary: %v", rData)
	}
}
