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
	return runVerificationContext(t, cfg, context.Background(), steps...)
}
func runVerificationContext(t *testing.T, cfg *Config, ctx context.Context, steps ...string) ([]Event, []string, error) {
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
	_, _, err := cfg.Run(ctx, []llm.Message{{Role: "user", Content: "work"}})
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
	cfg.verificationTouch(context.Background())
	if err := os.WriteFile(filepath.Join(cfg.Exec.Root, "input"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	attempt := cfg.verificationStart(context.Background(), "bash", cfg.verificationArguments("bash", `{"command":"true"}`))
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

func TestRequiredVerificationQuestionInUnsupportedWorkspace(t *testing.T) {
	for _, read := range []bool{false, true} {
		t.Run(fmt.Sprint(read), func(t *testing.T) {
			cfg := verificationConfig(t, "true")
			cfg.Exec.Plugins = &fakePluginRunner{}
			if err := os.WriteFile(filepath.Join(cfg.Exec.Root, "input"), []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("input", filepath.Join(cfg.Exec.Root, "alias")); err != nil {
				t.Skip(err)
			}
			steps := []string{textSSE("answer to question")}
			if read {
				steps = append([]string{verificationTool("read", map[string]any{"path": "input"})}, steps...)
			}
			_, _, err := runVerification(t, cfg, steps...)
			if err != nil || cfg.LastTaskOutcome != "" {
				t.Fatalf("question wrongly required verification: outcome=%s err=%v", cfg.LastTaskOutcome, err)
			}
		})
	}
}

func TestRequiredVerificationRetargetedPolicyCwdDoesNotRelabelExecution(t *testing.T) {
	command := "test -f pass && ln -sfn second ../link"
	cfg := verificationConfig(t, command)
	for _, dir := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(cfg.Exec.Root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Exec.Root, "first", "pass"), []byte("pass"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("first", filepath.Join(cfg.Exec.Root, "link")); err != nil {
		t.Skip(err)
	}
	cfg.Verification.Required[0].Cwd = "link"
	cfg.Verification.Exclude = []string{"link"}
	_, _, err := runVerification(t, cfg, verificationWrite("input", "changed"), verificationTool("bash", map[string]any{"command": command, "cwd": "first"}))
	requireVerificationFailure(t, cfg, err)
	want, err := (verification.Check{Name: "test", Command: command, Cwd: "first"}).Directory(cfg.Exec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if record := cfg.verifier.latest["test"]; record.Cwd != want {
		t.Fatalf("execution relabeled: %+v", record)
	}
}

func TestRequiredVerificationIncludesOtherSessionsRunningJobs(t *testing.T) {
	cfg := verificationConfig(t, "true")
	cfg.Exec.SwitchSession("first")
	job, err := cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { job.Cancel(); <-job.Done() }()
	cfg.Exec.SwitchSession("second")
	_, _, err = runVerification(t, cfg, verificationWrite("input", "changed"), verificationBash("true"))
	requireVerificationFailure(t, cfg, err)
	if len(cfg.Exec.Jobs()) != 0 {
		t.Fatal("other session job output leaked")
	}
}

func TestRequiredVerificationUnchangedChecksDoNotClaimCompletion(t *testing.T) {
	for _, command := range []string{"true", "exit 1"} {
		t.Run(command, func(t *testing.T) {
			cfg := verificationConfig(t, command)
			_, bodies, err := runVerification(t, cfg, verificationBash(command))
			if err != nil || cfg.LastTaskOutcome != "" || len(bodies) != 2 {
				t.Fatalf("read-only outcome=%s calls=%d err=%v", cfg.LastTaskOutcome, len(bodies), err)
			}
		})
	}
}

type verificationCaptureContext struct {
	context.Context
	onErr func()
}

func (c verificationCaptureContext) Err() error { c.onErr(); return c.Context.Err() }

func TestRequiredVerificationJobFinishingAfterSnapshotCannotCertifyStaleFiles(t *testing.T) {
	cfg := verificationConfig(t, "true")
	ctx := context.Background()
	cfg.verifier = &verificationRun{policy: cfg.Verification, latest: map[string]verification.Evidence{}}
	cfg.verificationTouch(ctx)
	if err := os.WriteFile(filepath.Join(cfg.Exec.Root, "input"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	attempt := cfg.verificationStart(ctx, "bash", cfg.verificationArguments("bash", `{"command":"true"}`))
	result, err := cfg.Exec.Dispatch(ctx, "bash", `{"command":"true"}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.verificationFinish(ctx, attempt, result, err)
	trigger := filepath.Join(t.TempDir(), "release")
	job, err := cfg.Exec.BashBackground(ctx, tools.BashArgs{Command: fmt.Sprintf("while test ! -f %q; do sleep 0.01; done; printf changed > input", trigger), Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { job.Cancel(); <-job.Done() }()
	calls := 0
	if _, err := verification.Capture(verificationCaptureContext{Context: ctx, onErr: func() { calls++ }}, cfg.Exec.Root, nil); err != nil {
		t.Fatal(err)
	}
	seen := 0
	// If the gate captures while a writer is live, release that writer just
	// after the terminal version is formed. Its new bytes must not be certified
	// using the old snapshot merely because it has finished by the job scan.
	gateCtx := verificationCaptureContext{Context: ctx, onErr: func() {
		seen++
		if seen == calls {
			if err := os.WriteFile(trigger, []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case <-job.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("background writer did not finish")
			}
		}
	}}
	outcome, missing := cfg.verificationGate(gateCtx)
	if outcome == OutcomeCompleted || missing == "" {
		t.Fatalf("stale completion=%s missing=%s", outcome, missing)
	}
}

func TestRequiredVerificationUsesBashLexicalCwdNormalization(t *testing.T) {
	for _, explicitPolicy := range []bool{false, true} {
		t.Run(fmt.Sprint(explicitPolicy), func(t *testing.T) {
			command := "test -f root-only"
			cfg := verificationConfig(t, command)
			if err := os.MkdirAll(filepath.Join(cfg.Exec.Root, "sub", "deep"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("sub/deep", filepath.Join(cfg.Exec.Root, "link")); err != nil {
				t.Skip(err)
			}
			cfg.Verification.Exclude = []string{"link"}
			// Preserve the absolute link/.. spelling: filepath.Join would clean it.
			actual := cfg.Exec.Root + string(filepath.Separator) + "link" + string(filepath.Separator) + ".."
			cfg.Verification.Required[0].Cwd = "sub"
			if explicitPolicy {
				cfg.Verification.Required[0].Cwd = actual
			}
			_, _, err := runVerification(t, cfg, verificationWrite("root-only", "present only at root"), verificationTool("bash", map[string]any{"command": command, "cwd": actual}))
			if explicitPolicy {
				if err != nil || cfg.LastTaskOutcome != OutcomeCompleted {
					t.Fatalf("actual root check outcome=%s err=%v", cfg.LastTaskOutcome, err)
				}
			} else {
				requireVerificationFailure(t, cfg, err)
			}
		})
	}
}

type verificationMutatingStop struct {
	fakePluginRunner
	root string
}

func (h *verificationMutatingStop) RunStopHooks(context.Context, string, string) string {
	_ = os.WriteFile(filepath.Join(h.root, "input"), []byte("changed by hook"), 0600)
	_ = os.Symlink("input", filepath.Join(h.root, "alias"))
	return ""
}
func TestRequiredVerificationHookMakesSupportedWorkspaceUnverifiable(t *testing.T) {
	cfg := verificationConfig(t, "true")
	cfg.Exec.Plugins = &verificationMutatingStop{root: cfg.Exec.Root}
	_, _, err := runVerification(t, cfg, textSSE("answer"))
	requireVerificationFailure(t, cfg, err)
}

func TestRequiredVerificationBindsCwdBeforeApprovalAndSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := verificationConfig(t, "test -f pass")
	store, err := session.Open("cwd-binding", session.OpenOptions{Workspace: cfg.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg.Session = store
	for _, dir := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(cfg.Exec.Root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Exec.Root, "second", "pass"), []byte("wrong cwd passes"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.Exec.Root, "link")
	if err := os.Symlink("first", link); err != nil {
		t.Skip(err)
	}
	cfg.Verification.Required[0].Cwd = "first"
	cfg.Verification.Exclude = []string{"link"}
	armed := false
	var approved string
	cfg.Mode = "suggest"
	cfg.Ask = func(req ApprovalRequest) (bool, error) {
		if req.ToolName == "bash" {
			history, err := session.Load(store.ID)
			if err != nil {
				t.Fatal(err)
			}
			var last llm.Message
			for _, message := range history {
				if message.Role == "assistant" && len(message.ToolCalls) > 0 && message.ToolCalls[0].Function.Name == "bash" {
					last = message
				}
			}
			if len(last.ToolInvocations) != 1 || last.ToolInvocations[0].Arguments != req.Arguments {
				t.Fatal("canonical cwd not durable before approval")
			}
			var original tools.BashArgs
			if err := json.Unmarshal([]byte(last.ToolInvocations[0].OriginalArguments), &original); err != nil || original.Cwd != "link" {
				t.Fatal("original alias was lost")
			}
			armed = true
			approved = req.Arguments
		}
		return true, nil
	}
	ctx := verificationCaptureContext{Context: context.Background(), onErr: func() {
		if !armed {
			return
		}
		armed = false
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("second", link); err != nil {
			t.Fatal(err)
		}
	}}
	_, _, err = runVerificationContext(t, cfg, ctx, verificationWrite("input", "work"), verificationTool("bash", map[string]any{"command": "test -f pass", "cwd": "link"}))
	requireVerificationFailure(t, cfg, err)
	var actual tools.BashArgs
	if err := json.Unmarshal([]byte(approved), &actual); err != nil {
		t.Fatal(err)
	}
	wanted, err := (verification.Check{Name: "check", Command: "true", Cwd: "first"}).Directory(cfg.Exec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Cwd != wanted {
		t.Fatalf("approved unbound cwd: %s", approved)
	}
}

func TestRequiredVerificationCancellationAtTerminalGateIsAborted(t *testing.T) {
	cfg := verificationConfig(t, "true")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventStepUsage {
			cancel()
		}
	}
	events, _, err := runVerificationContext(t, cfg, ctx, textSSE("answer"))
	if err == nil || cfg.LastTaskOutcome != OutcomeAborted {
		t.Fatalf("outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
	for _, event := range events {
		if event.Kind == EventDone {
			t.Fatal("cancelled gate ended successfully")
		}
	}
}
