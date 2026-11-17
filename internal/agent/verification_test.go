package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
)

func verificationConfig(t *testing.T, command string) *Config {
	t.Helper()
	return &Config{Exec: &tools.Executor{Root: t.TempDir()}, Tools: ToolDefs(), Mode: "auto", MaxSteps: 20,
		Verification: &verification.Policy{Required: []verification.Check{{Name: "test", Command: command}}}}
}

func verificationTool(name string, args any) string {
	data, _ := json.Marshal(args)
	return toolCallSSE("call", name, string(data))
}
func verificationWrite(path, content string) string {
	return verificationTool("write", map[string]any{"path": path, "content": content, "overwrite": true})
}
func verificationBash(command string) string {
	return verificationTool("bash", map[string]any{"command": command})
}
func runVerification(t *testing.T, cfg *Config, steps ...string) ([]Event, []string, error) {
	t.Helper()
	var calls atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		n := int(calls.Add(1)) - 1
		if n < len(steps) {
			fmt.Fprint(w, steps[n])
		} else {
			fmt.Fprint(w, textSSE("done"))
		}
	}))
	defer srv.Close()
	cfg.Client = &llm.Client{API: "chat", APIKey: "fake", Model: "fake", BaseURL: srv.URL}
	var events []Event
	prev := cfg.OnEvent
	cfg.OnEvent = func(e Event) {
		events = append(events, e)
		if prev != nil {
			prev(e)
		}
	}
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "work"}})
	return events, bodies, err
}
func requireVerificationFailure(t *testing.T, cfg *Config, err error) {
	t.Helper()
	if err == nil || cfg.LastTerminalReason != TerminalVerificationFailed || cfg.LastTaskOutcome != OutcomeVerificationFailed {
		t.Fatalf("reason=%s outcome=%s err=%v", cfg.LastTerminalReason, cfg.LastTaskOutcome, err)
	}
}

func TestRequiredVerificationCompletesAndPersistsEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "must-not-reach-shell")
	command := `test -z "$OPENAI_API_KEY" && test -f input.txt`
	cfg := verificationConfig(t, command)
	cfg.Exec.BackgroundThreshold = time.Nanosecond
	// Both policy names refer to the same execution, including a cwd alias.
	cfg.Verification.Required = append(cfg.Verification.Required, verification.Check{Name: "alias", Command: command, Cwd: "."})
	store, err := session.Open("verified", session.OpenOptions{Workspace: cfg.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg.Session = store
	events, bodies, err := runVerification(t, cfg, verificationWrite("input.txt", "ready"), verificationBash(command))
	if err != nil || cfg.LastTaskOutcome != OutcomeCompleted {
		t.Fatalf("outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
	for _, e := range events {
		if e.Kind == EventToolBackgrounded {
			t.Fatal("required check automatically backgrounded")
		}
	}
	if events[len(events)-1].TaskOutcome != OutcomeCompleted {
		t.Fatal("missing completed event")
	}
	if !strings.Contains(bodies[len(bodies)-1], "harness verification evidence") {
		t.Fatal("model did not receive evidence")
	}
	history, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	for _, msg := range history {
		if msg.Kind != "verification" || !strings.HasPrefix(msg.Content, "[harness verification evidence]\n") {
			continue
		}
		var record verification.Evidence
		if err := json.Unmarshal([]byte(strings.TrimPrefix(msg.Content, "[harness verification evidence]\n")), &record); err != nil {
			t.Fatal(err)
		}
		if record.InvocationID == "" || record.ToolCallID == "" || record.ExitCode == nil || *record.ExitCode != 0 || record.Version == "" || record.Cwd == "" || record.Finished.Before(record.Started) {
			t.Fatalf("invalid record: %+v", record)
		}
		records++
	}
	if records != 2 {
		t.Fatalf("records=%d", records)
	}
	// Evidence is scoped to one Run; a subsequent question makes no completion claim.
	cfg.Session = nil
	cfg.OnEvent = nil
	_, _, err = runVerification(t, cfg, textSSE("answer"))
	if err != nil || cfg.LastTaskOutcome != "" {
		t.Fatalf("stale turn outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
}

func TestRequiredVerificationRejectsMissingFailedAndStaleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		steps         []string
	}{
		{"missing", "true", []string{verificationWrite("input", "one")}},
		{"failed", "exit 1", []string{verificationWrite("input", "one"), verificationBash("exit 1")}},
		{"later edit", "true", []string{verificationWrite("input", "one"), verificationBash("true"), verificationWrite("input", "two")}},
		{"check changes input", "printf changed > input", []string{verificationWrite("input", "one"), verificationBash("printf changed > input")}},
		{"wrong command", "true", []string{verificationWrite("input", "one"), verificationBash("printf passed")}},
		{"wrong cwd", "true", []string{verificationWrite("sub/input", "one"), verificationTool("bash", map[string]any{"command": "true", "cwd": "sub"})}},
		{"background", "true", []string{verificationWrite("input", "one"), verificationTool("bash", map[string]any{"command": "true", "background": true})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := verificationConfig(t, tc.command)
			_, bodies, err := runVerification(t, cfg, tc.steps...)
			requireVerificationFailure(t, cfg, err)
			if len(bodies) > len(tc.steps)+maxVerificationFeedback+2 {
				t.Fatalf("unbounded feedback: %d", len(bodies))
			}
		})
	}
}

func TestRequiredVerificationFeedbackCanBeResolved(t *testing.T) {
	cfg := verificationConfig(t, "true")
	_, bodies, err := runVerification(t, cfg, verificationWrite("input", "one"), textSSE("done"), verificationBash("true"))
	if err != nil || cfg.LastTaskOutcome != OutcomeCompleted {
		t.Fatalf("outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
	if !strings.Contains(bodies[2], "run Bash command") {
		t.Fatal("missing actionable feedback")
	}
}

func TestRequiredVerificationRetainsLatestFailureAtSameVersion(t *testing.T) {
	command := "test ! -f output/fail"
	cfg := verificationConfig(t, command)
	cfg.Verification.Exclude = []string{"output"}
	if err := os.Mkdir(filepath.Join(cfg.Exec.Root, "output"), 0700); err != nil {
		t.Fatal(err)
	}
	_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationBash(command), verificationWrite("output/fail", "fail"), verificationBash(command))
	requireVerificationFailure(t, cfg, err)
	record := cfg.verifier.latest["test"]
	state, captureErr := verification.Capture(context.Background(), cfg.Exec.Root, cfg.Verification.Exclude)
	if captureErr != nil {
		t.Fatal(captureErr)
	}
	if record.Version != state.Version || record.ExitCode == nil || *record.ExitCode == 0 {
		t.Fatalf("did not retain same-version failure: %+v", record)
	}
}

func TestRequiredVerificationUsesFinalArgsAndRawResult(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprint(rewrite), func(t *testing.T) {
			command := "exit 1"
			cfg := verificationConfig(t, command)
			hooks := &fakePluginRunner{result: map[string]string{"bash": "all checks passed"}}
			if rewrite {
				command = "true"
				cfg.Verification.Required[0].Command = command
				hooks.rewrite = map[string]string{"bash": `{"command":"printf fake"}`}
			}
			cfg.Exec.Plugins = hooks
			_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationBash(command))
			requireVerificationFailure(t, cfg, err)
		})
	}
}

func TestRequiredVerificationHonorsApprovalAndTimeout(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		cfg := verificationConfig(t, "printf ran > sentinel")
		cfg.Mode = "suggest"
		cfg.Ask = func(req ApprovalRequest) (bool, error) { return req.ToolName != "bash", nil }
		_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationBash(cfg.Verification.Required[0].Command))
		requireVerificationFailure(t, cfg, err)
		if _, err := os.Stat(filepath.Join(cfg.Exec.Root, "sentinel")); !os.IsNotExist(err) {
			t.Fatal("denied check executed")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		cfg := verificationConfig(t, "sleep 2")
		_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationTool("bash", map[string]any{"command": "sleep 2", "timeout": 1}))
		requireVerificationFailure(t, cfg, err)
		if !strings.Contains(cfg.verifier.latest["test"].Error, "timed out") {
			t.Fatal("timeout lost")
		}
	})
}

func TestRequiredVerificationUnchangedReadDoesNotClaimCompletion(t *testing.T) {
	cfg := verificationConfig(t, "true")
	if err := os.WriteFile(filepath.Join(cfg.Exec.Root, "input"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runVerification(t, cfg, verificationTool("read", map[string]any{"path": "input"}))
	if err != nil || cfg.LastTaskOutcome != "" {
		t.Fatalf("outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
}

func TestRequiredVerificationCaptureErrorFailsClosed(t *testing.T) {
	cfg := verificationConfig(t, "true")
	if err := os.Symlink("input", filepath.Join(cfg.Exec.Root, "alias")); err != nil {
		t.Skip(err)
	}
	_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationBash("true"))
	requireVerificationFailure(t, cfg, err)
	if !strings.Contains(err.Error(), "Cannot verify workspace version") {
		t.Fatal(err)
	}
}

func TestRequiredVerificationWhitespaceCwdUsesActualBashDefault(t *testing.T) {
	cfg := verificationConfig(t, "test -f input")
	cfg.Verification.Required[0].Cwd = "  "
	_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationTool("bash", map[string]any{"command": "test -f input", "cwd": "  "}))
	if err != nil || cfg.LastTaskOutcome != OutcomeCompleted {
		t.Fatalf("outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
}

func TestRequiredVerificationWaitsForRunningJobs(t *testing.T) {
	cfg := verificationConfig(t, "true")
	cfg.verifier = &verificationRun{policy: cfg.Verification, latest: map[string]verification.Evidence{}}
	attempt := cfg.verificationStart(context.Background(), "bash", `{"command":"true"}`)
	zero := 0
	cfg.verificationFinish(context.Background(), attempt, tools.ToolResult{ExitCode: &zero}, nil)
	job, err := cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { job.Cancel(); <-job.Done() }()
	outcome, missing := cfg.verificationGate(context.Background())
	if outcome != "" || !strings.Contains(missing, "Background work") {
		t.Fatalf("outcome=%s missing=%s", outcome, missing)
	}
}

func TestRequiredVerificationRequiresEveryCheck(t *testing.T) {
	cfg := verificationConfig(t, "true")
	cfg.Verification.Required = append(cfg.Verification.Required, verification.Check{Name: "second", Command: "printf second"})
	_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationBash("true"))
	requireVerificationFailure(t, cfg, err)
	if !strings.Contains(err.Error(), "second") {
		t.Fatal("missing check not reported")
	}
}

func TestRequiredVerificationAllowsExplicitGeneratedOutputs(t *testing.T) {
	command := "mkdir -p output; printf pass > output/result"
	cfg := verificationConfig(t, command)
	cfg.Verification.Exclude = []string{"output"}
	_, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationBash(command))
	if err != nil || cfg.LastTaskOutcome != OutcomeCompleted {
		t.Fatalf("outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
}

func TestRequiredVerificationPersistenceFailureCannotComplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := verificationConfig(t, "true")
	store, err := session.Open("failed-evidence", session.OpenOptions{Workspace: cfg.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg.Session = store
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolName == "bash" {
			store.Close()
		}
	}
	events, _, err := runVerification(t, cfg, verificationWrite("input", "one"), verificationBash("true"))
	if err == nil || cfg.LastTaskOutcome != OutcomeIncomplete {
		t.Fatalf("outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
	for _, event := range events {
		if event.TaskOutcome == OutcomeCompleted {
			t.Fatal("completion published before evidence persisted")
		}
	}
}
