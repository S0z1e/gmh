package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/failureguard"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestFailureProgressRerunsRepairedCommandAfterThreeFailures(t *testing.T) {
	c := budgetConfig(t, nil)
	command := `printf x >> attempts; test -f ready`
	fail := verificationBash(command)
	events, _, err := runVerification(t, c,
		fail, verificationTool("write", map[string]any{"path": "source", "content": "one"}),
		fail, verificationTool("write", map[string]any{"path": "source", "content": "two", "overwrite": true}),
		fail, verificationTool("write", map[string]any{"path": "ready", "content": "fixed"}),
		fail, textSSE("done"))
	if err != nil || c.LastTerminalReason != TerminalEndTurn {
		t.Fatalf("repaired command could not finish: reason=%s err=%v", c.LastTerminalReason, err)
	}
	data, err := os.ReadFile(filepath.Join(c.Exec.Root, "attempts"))
	if err != nil || string(data) != "xxxx" {
		t.Fatalf("fixed version was not checked: attempts=%q err=%v events=%+v", data, err, events)
	}
}

func TestFailureProgressReadNoopAndUnrelatedSuccessCannotResetStall(t *testing.T) {
	for _, harmless := range []string{
		verificationTool("read", map[string]any{"path": "stable"}),
		verificationTool("write", map[string]any{"path": "stable", "content": "same", "overwrite": true}),
		verificationBash("true"),
	} {
		t.Run(harmless, func(t *testing.T) {
			c := budgetConfig(t, nil)
			if err := os.WriteFile(filepath.Join(c.Exec.Root, "stable"), []byte("same"), 0600); err != nil {
				t.Fatal(err)
			}
			fail := verificationBash("printf x >> attempts; exit 1")
			events, bodies, err := runVerification(t, c, fail, harmless, fail, harmless, fail, textSSE("blocked report"))
			if err == nil || c.LastTerminalReason != TerminalBlocked || c.LastTaskOutcome != OutcomeBlocked || len(bodies) != 6 || countKind(events, EventError) != 1 {
				t.Fatalf("no-progress loop escaped: reason=%s requests=%d err=%v", c.LastTerminalReason, len(bodies), err)
			}
			data, err := os.ReadFile(filepath.Join(c.Exec.Root, "attempts"))
			if err != nil || string(data) != "xxx" {
				t.Fatalf("unexpected failing executions: %q err=%v", data, err)
			}
		})
	}
}

func TestFailureProgressBackgroundFailureUsesSameGuard(t *testing.T) {
	c := budgetConfig(t, nil)
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolBackgrounded {
			for _, job := range c.Exec.Jobs() {
				if job.ID == e.JobID {
					select {
					case <-job.Done():
					case <-time.After(3 * time.Second):
						t.Fatal("background fixture did not exit")
					}
				}
			}
		}
	}
	fail := verificationTool("bash", map[string]any{"command": "printf x >> attempts; exit 1", "background": true})
	events, _, err := runVerification(t, c, fail, fail, fail, fail, textSSE("blocked report"))
	if err == nil || c.LastTerminalReason != TerminalBlocked || countKind(events, EventToolBackgroundDone) != 3 {
		t.Fatalf("background failures escaped: reason=%s events=%+v err=%v", c.LastTerminalReason, events, err)
	}
	data, err := os.ReadFile(filepath.Join(c.Exec.Root, "attempts"))
	if err != nil || string(data) != "xxx" {
		t.Fatalf("failed background command ran again: %q err=%v", data, err)
	}
	denied := false
	for _, e := range events {
		if e.Kind == EventToolEnd && strings.Contains(e.ToolOutput, RepeatedToolFailureReason) {
			denied = true
		}
	}
	if !denied {
		t.Fatal("background result did not reach the invocation's breaker fingerprint")
	}
}

func TestFailureProgressUnknownStateCannotInventRepair(t *testing.T) {
	c := budgetConfig(t, nil)
	if err := os.Symlink(t.TempDir(), filepath.Join(c.Exec.Root, "unsupported-input")); err != nil {
		t.Skip(err)
	}
	fail := verificationBash("exit 1")
	_, _, err := runVerificationContext(t, c, context.Background(), fail,
		verificationTool("write", map[string]any{"path": "one", "content": "one"}), fail,
		verificationTool("write", map[string]any{"path": "two", "content": "two"}), fail,
		textSSE("blocked report"))
	if err == nil || c.LastTerminalReason != TerminalBlocked {
		t.Fatalf("unsupported snapshot invented progress: reason=%s err=%v", c.LastTerminalReason, err)
	}
}

func TestFailureKindsDistinguishPolicyTimeoutEnvironmentAndCheck(t *testing.T) {
	for _, tc := range []struct {
		err   error
		check bool
		want  failureguard.Kind
	}{
		{&tools.PolicyError{Reason: "local restriction"}, true, failureguard.PolicyDenied},
		{context.DeadlineExceeded, true, failureguard.Transient},
		{os.ErrNotExist, false, failureguard.Environment},
		{os.ErrPermission, true, failureguard.VerificationFailure},
	} {
		if got := failureKind(tc.err, tc.check); got != tc.want {
			t.Fatalf("%v check=%v: got %s want %s", tc.err, tc.check, got, tc.want)
		}
	}
}

func TestFailureProgressCompletedJobCannotMakeUnrelatedCommandProgress(t *testing.T) {
	c := budgetConfig(t, nil)
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolBackgrounded {
			for _, job := range c.Exec.Jobs() {
				if job.ID == e.JobID {
					select {
					case <-job.Done():
					case <-time.After(3 * time.Second):
						t.Fatal("fixture did not finish")
					}
				}
			}
		}
	}
	first := []llm.ToolCall{budgetCall("a", "bash", `{"command":"exit 1"}`), budgetCall("b", "bash", `{"command":"exit 1"}`)}
	second := []llm.ToolCall{budgetCall("job", "bash", `{"command":"printf failure > failure-output; exit 1","background":true}`), budgetCall("unrelated", "bash", `{"command":"true"}`)}
	events, bodies, err := runVerification(t, c, budgetSSE("", first, nil, true), budgetSSE("", second, nil, true), textSSE("blocked report"))
	if err == nil || c.LastTerminalReason != TerminalBlocked || len(bodies) != 3 || countKind(events, EventToolBackgroundDone) != 1 {
		t.Fatalf("failed job output invented progress: reason=%s requests=%d err=%v", c.LastTerminalReason, len(bodies), err)
	}
}

func TestFailureProgressToolFreeDrainRechecksBlocked(t *testing.T) {
	for _, finishDuringDrain := range []bool{false, true} {
		t.Run(fmt.Sprint(finishDuringDrain), func(t *testing.T) {
			c := budgetConfig(t, nil)
			c.DrainTimeout = 20 * time.Millisecond
			defer c.Exec.CancelJobs()
			c.OnEvent = func(e Event) {
				if e.Kind == EventToolEnd && e.ToolCallID == "pending" {
					if err := os.WriteFile(filepath.Join(c.Exec.Root, "failures-release"), nil, 0600); err != nil {
						t.Fatal(err)
					}
					for _, job := range c.Exec.Jobs() {
						if job.ToolCallID != "pending" {
							select {
							case <-job.Done():
							case <-time.After(3 * time.Second):
								t.Fatal("fixture did not finish")
							}
						}
					}
				}
				if e.Kind == EventBackgroundWait && finishDuringDrain {
					if err := os.WriteFile(filepath.Join(c.Exec.Root, "release"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			var calls []llm.ToolCall
			for _, id := range []string{"a", "b", "c"} {
				calls = append(calls, budgetCall(id, "bash", `{"command":"while [ ! -f failures-release ]; do sleep 0.01; done; exit 1","background":true}`))
			}
			calls = append(calls, budgetCall("pending", "bash", `{"command":"while [ ! -f release ]; do sleep 0.01; done; exit 1","background":true}`))
			_, bodies, err := runVerification(t, c, budgetSSE("", calls, nil, true), textSSE("no more tools"), textSSE("blocked report"))
			if err == nil || c.LastTerminalReason != TerminalBlocked || len(bodies) != 3 || strings.Contains(bodies[2], `"tools"`) {
				t.Fatalf("ordinary request after failed drain: reason=%s requests=%d third=%s err=%v", c.LastTerminalReason, len(bodies), bodies[min(2, len(bodies)-1)], err)
			}
		})
	}
}

func TestFailureProgressRuntimeInputErrorsCannotBeRepairedByEdit(t *testing.T) {
	calls := []llm.ToolCall{
		budgetCall("edit-range", "edit", `{"path":"missing","startLine":3,"endLine":1,"newText":"fixed"}`),
		budgetCall("glob", "glob", `{"pattern":""}`),
		budgetCall("grep", "grep", `{"pattern":""}`),
		budgetCall("bash", "bash", `{"command":"true","timeout":1.0}`),
		budgetCall("question", "ask_user", `{"questions":[{"question":"choose","options":[{"label":"same"},{"label":"same"}]}]}`),
	}
	if _, err := exec.LookPath("rg"); err == nil {
		calls = append(calls, budgetCall("bad-glob", "glob", `{"pattern":"["}`), budgetCall("bad-regex", "grep", `{"pattern":"["}`))
	}
	for _, call := range calls {
		t.Run(call.ID, func(t *testing.T) {
			c := budgetConfig(t, nil)
			ctx := context.Background()
			c.progress = &turnProgress{ctx: ctx, jobs: make(map[string]bool)}
			key := CanonicalToolKey(call.Function.Name, call.Function.Arguments)
			for i := 0; i < 3; i++ {
				var err error
				if call.Function.Name == "ask_user" {
					_, err = c.askUser(ctx, call.Function.Arguments)
				} else if call.Function.Name == "bash" {
					_, _, err = c.execBash(ctx, call)
				} else {
					_, err = c.Exec.Dispatch(ctx, call.Function.Name, call.Function.Arguments)
				}
				if err == nil {
					t.Fatal("invalid fixture was accepted")
				}
				c.progressFailure(key, failureKind(err, false))
			}
			if err := os.WriteFile(filepath.Join(c.Exec.Root, "unrelated"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if d := c.progressAdmit(key); !d.Denied || d.Kind != failureguard.InvalidInput {
				t.Fatalf("invalid runtime input admitted a state probe: %+v", d)
			}
		})
	}
}

func TestFailureProgressHarnessBookkeepingCannotProveRepair(t *testing.T) {
	for _, separateOutput := range []bool{false, true} {
		t.Run(fmt.Sprint(separateOutput), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			c := budgetConfig(t, nil)
			c.Exec.Root = root
			store, err := session.Open("progress-in-home", session.OpenOptions{Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			c.Session = store
			c.Exec.Audit = func(e tools.AuditEntry) { _ = store.Audit(e) }
			c.Exec.OutputDir = filepath.Join(session.Dir(), "outputs")
			if separateOutput {
				c.Exec.OutputDir = filepath.Join(root, "custom-output")
			}
			c.progress = &turnProgress{ctx: context.Background(), jobs: make(map[string]bool)}
			if revision := c.progressRevision(); revision == "" {
				t.Fatal("a bounded workspace containing bookkeeping must still have usable proof")
			}
			c.progress = nil
			fail := verificationBash("exit 1")
			events, _, err := runVerification(t, c, fail, verificationBash("true"), fail, verificationBash("true"), fail, textSSE("blocked report"))
			if err == nil || c.LastTerminalReason != TerminalBlocked {
				t.Fatalf("transcript/audit/output updates invented progress: reason=%s err=%v events=%d", c.LastTerminalReason, err, len(events))
			}
			// Likewise, recording four invocations in one accepted batch must
			// not turn the fourth identical attempt into a state-change probe.
			var batch []llm.ToolCall
			for _, id := range []string{"a", "b", "c", "d"} {
				batch = append(batch, budgetCall(id, "bash", `{"command":"exit 1"}`))
			}
			events, _, err = runVerification(t, c, budgetSSE("", batch, nil, true), textSSE("blocked report"))
			denied := false
			for _, e := range events {
				if e.Kind == EventToolEnd && e.ToolCallID == "d" && strings.Contains(e.ToolOutput, RepeatedToolFailureReason) {
					denied = true
				}
			}
			if err == nil || !denied {
				t.Fatalf("invocation bookkeeping reopened the fourth attempt: denied=%v err=%v", denied, err)
			}
		})
	}
}

func TestFailureProgressBookkeepingAliasesPreserveProductChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	c := budgetConfig(t, nil)
	c.Exec.Root = alias
	c.Exec.OutputDir = filepath.Join(alias, "custom-output")
	c.progress = &turnProgress{ctx: context.Background(), jobs: make(map[string]bool)}
	before := c.progressRevision()
	if before == "" {
		t.Fatal("aliased workspace produced unknown proof")
	}
	for _, file := range []string{filepath.Join(session.Dir(), "transcript"), filepath.Join(c.Exec.OutputDir, "output")} {
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("bookkeeping"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if after := c.progressRevision(); after != before {
		t.Fatalf("aliased bookkeeping changed product proof: before=%s after=%s", before, after)
	}
	if err := os.WriteFile(filepath.Join(root, "product"), []byte("fixed"), 0600); err != nil {
		t.Fatal(err)
	}
	if after := c.progressRevision(); after == "" || after == before {
		t.Fatal("product change was hidden with bookkeeping")
	}
	c.Exec.OutputDir = root
	if after := c.progressRevision(); after != "" {
		t.Fatal("overlapping product/output root fabricated a usable version")
	}
}

func TestFailureProgressStoppedJobOutputCannotProveRepair(t *testing.T) {
	c := budgetConfig(t, nil)
	c.progress = &turnProgress{ctx: context.Background(), jobs: make(map[string]bool)}
	c.progressFailure("first", failureguard.Environment)
	c.progressFailure("second", failureguard.Environment)
	call := budgetCall("job", "bash", `{"command":"printf failed > job-output; while :; do sleep 1; done","background":true}`)
	ctx := tools.WithFailureKey(context.Background(), CanonicalToolKey("bash", call.Function.Arguments))
	_, job, err := c.execBash(ctx, call)
	if err != nil || job == nil {
		t.Fatalf("launch: %v", err)
	}
	defer c.Exec.CancelJobs()
	deadline := time.After(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(c.Exec.Root, "job-output")); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("job did not write fixture")
		case <-time.After(time.Millisecond):
		}
	}
	stop := budgetCall("stop", "bash", fmt.Sprintf(`{"killJobId":%q}`, job.ID))
	if _, _, err := c.execBash(context.Background(), stop); err != nil {
		t.Fatal(err)
	}
	c.progressSuccess(CanonicalToolKey("bash", stop.Function.Arguments), "bash")
	if c.progress.failures != 3 {
		t.Fatalf("stopped-job output invented progress: failures=%d", c.progress.failures)
	}
	c.progressReadyJobs()
	if c.progress.failures != 3 {
		t.Fatal("stopped job counted more than once")
	}
}

func TestFailureProgressPriorDeliveredJobsStayInPriorTurn(t *testing.T) {
	c := budgetConfig(t, nil)
	for i := 0; i < 3; i++ {
		job, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "exit 1"})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-job.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("fixture did not exit")
		}
		job.MarkDelivered()
	}
	if err := os.WriteFile(filepath.Join(c.Exec.Root, "source"), []byte("same"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runVerification(t, c, verificationTool("read", map[string]any{"path": "source"}), textSSE("done"))
	if err != nil || c.LastTerminalReason != TerminalEndTurn {
		t.Fatalf("prior delivered jobs entered fresh guard: %v", err)
	}
}
func TestFailureProgressWorkspaceWithinBookkeepingHasUnknownProof(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	output := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.MkdirAll(session.Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(session.Dir(), alias); err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct{ name, root, output string }{
		{"sessions", session.Dir(), ""},
		{"nested", filepath.Join(session.Dir(), "nested"), ""},
		{"alias", alias, ""},
		{"custom-output", filepath.Join(output, "nested"), output},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.root, 0700); err != nil {
				t.Fatal(err)
			}
			c := budgetConfig(t, nil)
			c.Exec.Root = tc.root
			c.Exec.OutputDir = tc.output
			c.progress = &turnProgress{ctx: context.Background(), jobs: make(map[string]bool)}
			if got := c.progressRevision(); got != "" {
				t.Fatalf("workspace inside harness bookkeeping invented usable proof: %s", got)
			}
		})
	}
}
