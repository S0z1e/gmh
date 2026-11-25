package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

type agentCrashMeta struct {
	SessionID      string `json:"session_id"`
	JobID          string `json:"job_id"`
	Started        string `json:"started"`
	OutputFile     string `json:"output_file"`
	PID            int    `json:"pid"`
	PGID           int    `json:"pgid"`
	ProcessStarted string `json:"process_started"`
}

func TestAgentCrashRecoveryHelperProcess(t *testing.T) {
	if os.Getenv("GMH_AGENT_CRASH_HELPER") != "1" {
		return
	}
	root := os.Getenv("GMH_HELPER_ROOT")
	cmdText := os.Getenv("GMH_HELPER_CMD")
	waitText := os.Getenv("GMH_HELPER_WAIT_TEXT")
	skipToolResult := os.Getenv("GMH_HELPER_SKIP_TOOL_RESULT") == "1"

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

	ctx := tools.WithInvocation(context.Background(), "inv-crash-1", "call_crash_1")
	job, err := exec.BashBackground(ctx, tools.BashArgs{
		Command:    cmdText,
		Background: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "BashBackground failed: %v\n", err)
		os.Exit(2)
	}

	userMsg := llm.Message{Role: "user", Content: "run background task"}
	assistantMsg := llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID:   "call_crash_1",
			Type: "function",
			Function: llm.FunctionCall{
				Name:      "bash",
				Arguments: fmt.Sprintf(`{"command":%q,"background":true}`, cmdText),
			},
		}},
	}
	if err := store.Append(userMsg); err != nil {
		os.Exit(2)
	}
	if err := store.Append(assistantMsg); err != nil {
		os.Exit(2)
	}
	if !skipToolResult {
		toolMsg := llm.Message{
			Role:         "tool",
			ToolCallID:   "call_crash_1",
			InvocationID: "inv-crash-1",
			Content:      fmt.Sprintf("Started background job %s", job.ID),
			Observation: &llm.ToolObservation{
				Tool:        "bash",
				Fingerprint: factDigest("bash:" + cmdText),
				Command:     cmdText,
				OutputFile:  job.OutputFile,
				Jobs:        []checkpoint.Job{snapshotJob(job, false)},
			},
		}
		if err := store.Append(toolMsg); err != nil {
			os.Exit(2)
		}
	}

	if waitText != "" && job.OutputFile != "" {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(job.OutputFile); err == nil && strings.Contains(string(data), waitText) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	snap := job.Snapshot()
	meta := agentCrashMeta{
		SessionID:      store.ID,
		JobID:          snap.ID,
		Started:        snap.Started.UTC().Format(time.RFC3339Nano),
		OutputFile:     snap.OutputFile,
		PID:            snap.Identity.PID,
		PGID:           snap.Identity.PGID,
		ProcessStarted: snap.Identity.StartToken,
	}
	line, _ := json.Marshal(meta)
	fmt.Println(string(line))
	select {}
}

func runAgentCrashHelper(t *testing.T, root, cmdText, waitText string, extraEnv ...string) agentCrashMeta {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestAgentCrashRecoveryHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GMH_AGENT_CRASH_HELPER=1",
		"GMH_HELPER_ROOT="+root,
		"GMH_HELPER_CMD="+cmdText,
		"GMH_HELPER_WAIT_TEXT="+waitText,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
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
		t.Fatalf("helper failed before emitting metadata: %v", sc.Err())
	}
	var meta agentCrashMeta
	if err := json.Unmarshal([]byte(sc.Text()), &meta); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("invalid metadata JSON: %v", err)
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
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return meta
}

func newTextMockServer(t *testing.T, reply string) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		_ = atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", reply)
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func countJobNotices(history []llm.Message, jobID string, status checkpoint.JobStatus) int {
	n := 0
	for _, m := range history {
		if m.Kind != "job" || m.Observation == nil {
			continue
		}
		for _, j := range m.Observation.Jobs {
			if j.ID == jobID && j.Status == status {
				n++
			}
		}
	}
	return n
}

func TestRecoverSessionDoesNotReplaySideEffectingCommand(t *testing.T) {
	for _, skipResult := range []bool{false, true} {
		name := "after_tool_result"
		if skipResult {
			name = "mid_turn_before_tool_result"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			sideEffectLog := filepath.Join(root, "side_effect.log")
			cmdText := fmt.Sprintf("printf 'executed\\n' >> %q; printf 'MARKER\\n'; sleep 0.2; exit 4", sideEffectLog)

			extra := []string{}
			if skipResult {
				extra = append(extra, "GMH_HELPER_SKIP_TOOL_RESULT=1")
			}
			meta := runAgentCrashHelper(t, root, cmdText, "MARKER", extra...)

			// Ensure subprocess exits before reopen so recovered status is unknown.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, _, alive := tools.ProcessIdentity(meta.PID); !alive {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}

			store, err := session.Open(meta.SessionID, session.OpenOptions{Workspace: root, Existing: true})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			history, err := session.Load(store.ID)
			if err != nil {
				t.Fatal(err)
			}

			srv, bodies := newTextMockServer(t, "I see the job state is unknown and will not replay it automatically.")
			exec := &tools.Executor{
				Root:                root,
				OutputDir:           store.OutputDir(),
				OutputID:            store.OutputID(),
				JobsDir:             store.JobsDir(),
				BackgroundThreshold: tools.DefaultBackgroundThreshold,
			}
			exec.SwitchSession(store.ID)
			defer exec.CloseJobs(context.Background())

			cfg := &Config{
				Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
				Exec:     exec,
				Session:  store,
				Tools:    ToolDefs(),
				System:   "test",
				Mode:     "auto",
				MaxSteps: 4,
				CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
					return "Summary retaining unknown job state.", llm.Usage{}, nil
				},
			}
			history = append(history, llm.Message{Role: "user", Content: "continue after crash"})
			_ = store.Append(history[len(history)-1])
			if _, _, err := cfg.Run(context.Background(), history); err != nil {
				t.Fatalf("cfg.Run failed: %v", err)
			}

			data, err := os.ReadFile(sideEffectLog)
			if err != nil {
				t.Fatal(err)
			}
			if lines := strings.Count(string(data), "executed\n"); lines != 1 {
				t.Fatalf("side-effecting command ran %d times across crash/reopen, want 1", lines)
			}
			if len(*bodies) == 0 || !strings.Contains((*bodies)[0], "has status unknown") || !strings.Contains((*bodies)[0], "Do not automatically replay") {
				t.Fatalf("model did not receive explicit unknown non-replay notice: %v", *bodies)
			}

			// Commit a real end-to-end compaction, delete JobsDir, reopen fresh,
			// and verify the unknown notice is STILL not delivered a second time.
			saved, err := session.Load(store.ID)
			if err != nil {
				t.Fatal(err)
			}
			compacted, _, err := cfg.Compact(context.Background(), saved)
			if err != nil {
				t.Fatalf("cfg.Compact failed: %v", err)
			}
			_ = os.RemoveAll(store.JobsDir())

			exec2 := &tools.Executor{
				Root:                root,
				OutputDir:           store.OutputDir(),
				OutputID:            store.OutputID(),
				JobsDir:             store.JobsDir(),
				BackgroundThreshold: tools.DefaultBackgroundThreshold,
			}
			exec2.SwitchSession(store.ID)
			defer exec2.CloseJobs(context.Background())
			if _, err := RecoverExecutorJobs(exec2, store.ID, compacted); err != nil {
				t.Fatal(err)
			}
			cfg.Exec = exec2
			compacted = append(compacted, llm.Message{Role: "user", Content: "turn after compaction"})
			_ = store.Append(compacted[len(compacted)-1])
			if _, _, err := cfg.Run(context.Background(), compacted); err != nil {
				t.Fatalf("post-compact cfg.Run failed: %v", err)
			}
			finalSaved, err := session.Load(store.ID)
			if err != nil {
				t.Fatal(err)
			}
			if count := countJobNotices(finalSaved, meta.JobID, checkpoint.Unknown); count != 0 {
				// Compacted checkpoint carries UnknownNoticed so no duplicate kind:job should be appended after compact.
				t.Fatalf("duplicate unknown notice appended after compaction: count=%d", count)
			}
			if err := SaveJobResults(store, exec2.Jobs()); err != nil {
				t.Fatal(err)
			}
			finalSaved2, err := session.Load(store.ID)
			if err != nil {
				t.Fatal(err)
			}
			if count := countJobNotices(finalSaved2, meta.JobID, checkpoint.Unknown); count != 0 {
				t.Fatalf("SaveJobResults duplicated unknown notice after compaction: count=%d", count)
			}
		})
	}
}

func TestNoDuplicateDeliveryAfterAdoptionAndCompaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	releaseFile := filepath.Join(root, "release")
	cmdText := fmt.Sprintf("printf 'ALIVE\\n'; while [ ! -f %q ]; do sleep 0.05; done; exit 5", releaseFile)

	meta := runAgentCrashHelper(t, root, cmdText, "ALIVE")

	store, err := session.Open(meta.SessionID, session.OpenOptions{Workspace: root, Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}

	exec := &tools.Executor{
		Root:                root,
		OutputDir:           store.OutputDir(),
		OutputID:            store.OutputID(),
		JobsDir:             store.JobsDir(),
		BackgroundThreshold: tools.DefaultBackgroundThreshold,
	}
	exec.SwitchSession(store.ID)
	defer exec.CloseJobs(context.Background())

	srv, bodies := newTextMockServer(t, "Acknowledged.")
	cfg := &Config{
		Client:       &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:         exec,
		Session:      store,
		Tools:        ToolDefs(),
		System:       "test",
		Mode:         "auto",
		MaxSteps:     4,
		DrainTimeout: 10 * time.Second,
		CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
			return "Compacted summary with adopted running and finished job facts.", llm.Usage{}, nil
		},
	}

	// Turn 1: job is adopted & still running -> delivers one running notice with
	// pipe-closure note and does NOT stall on drainPending.
	startTurn1 := time.Now()
	history = append(history, llm.Message{Role: "user", Content: "check status"})
	_ = store.Append(history[len(history)-1])
	if _, _, err := cfg.Run(context.Background(), history); err != nil {
		t.Fatalf("Turn 1 failed: %v", err)
	}
	if elapsed := time.Since(startTurn1); elapsed > 4*time.Second {
		t.Fatalf("Turn 1 stalled %v waiting on already-adopted running job", elapsed)
	}
	if len(*bodies) == 0 || !strings.Contains((*bodies)[0], "is still running (adopted PID") || !strings.Contains((*bodies)[0], "pipe closed") {
		t.Fatalf("Turn 1 missing adopted running notice / pipe closure note: %v", *bodies)
	}

	// Commit real compaction while adopted job is still running, wipe JobsDir, and reopen:
	// AdoptionNoticed in checkpoint must prevent duplicate running notices.
	savedMid, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	compactedMid, _, err := cfg.Compact(context.Background(), savedMid)
	if err != nil {
		t.Fatalf("mid-run Compact failed: %v", err)
	}
	_ = os.RemoveAll(store.JobsDir())
	reopenMidExec := &tools.Executor{Root: root, OutputDir: store.OutputDir(), OutputID: store.OutputID(), JobsDir: store.JobsDir()}
	reopenMidExec.SwitchSession(store.ID)
	defer reopenMidExec.CloseJobs(context.Background())
	if _, err := RecoverExecutorJobs(reopenMidExec, store.ID, compactedMid); err != nil {
		t.Fatal(err)
	}
	if !reopenMidExec.Jobs()[0].AdoptionNoticed() {
		t.Fatal("AdoptionNoticed should survive compaction via checkpoint even if JobsDir is cleared")
	}

	// Release adopted job and wait for exit on reopened executor.
	if err := os.WriteFile(releaseFile, []byte("go\n"), 0644); err != nil {
		t.Fatal(err)
	}
	job := reopenMidExec.Jobs()[0]
	select {
	case <-job.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for adopted job exit")
	}

	// Turn 2: delivers completion notice ONCE and populates OutputReceipt on checkpoint.
	cfg.Exec = reopenMidExec
	compactedMid = append(compactedMid, llm.Message{Role: "user", Content: "next turn"})
	_ = store.Append(compactedMid[len(compactedMid)-1])
	if _, _, err := cfg.Run(context.Background(), compactedMid); err != nil {
		t.Fatalf("Turn 2 failed: %v", err)
	}

	saved, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	var receiptFound bool
	for _, m := range saved {
		if m.Kind == "job" && m.Observation != nil {
			for _, cj := range m.Observation.Jobs {
				if cj.ID == meta.JobID && cj.Status == checkpoint.Done && cj.Delivered && cj.OutputReceipt != "" {
					receiptFound = true
				}
			}
		}
	}
	if !receiptFound {
		t.Fatal("completed job message did not persist OutputReceipt in checkpoint.Job")
	}
	if got := countJobNotices(saved, meta.JobID, checkpoint.Done); got != 1 {
		t.Fatalf("completion notice count = %d, want 1", got)
	}
}

func TestStopDeliveryOmitsUnknownExitCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	job := &tools.Job{ID: "bg-1", Started: time.Now()}
	ev := backgroundDoneEvent(job, tools.JobResult{ExitCode: -1, ExitCodeUnknown: true, Err: errors.New("exit unknown")}, "out", errors.New("exit unknown"))
	if ev.ToolExitCode != nil {
		t.Fatalf("expected nil ToolExitCode when ExitCodeUnknown=true, got %d", *ev.ToolExitCode)
	}
	_ = root
}
