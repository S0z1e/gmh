package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/failureguard"
	"minimalharness/internal/llm"
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
	for _, call := range []llm.ToolCall{
		budgetCall("glob", "glob", `{"pattern":""}`),
		budgetCall("grep", "grep", `{"pattern":""}`),
		budgetCall("question", "ask_user", `{"questions":[{"question":"choose","options":[{"label":"same"},{"label":"same"}]}]}`),
	} {
		t.Run(call.Function.Name, func(t *testing.T) {
			c := budgetConfig(t, nil)
			ctx := context.Background()
			c.progress = &turnProgress{ctx: ctx, jobs: make(map[string]bool)}
			key := CanonicalToolKey(call.Function.Name, call.Function.Arguments)
			for i := 0; i < 3; i++ {
				var err error
				if call.Function.Name == "ask_user" {
					_, err = c.askUser(ctx, call.Function.Arguments)
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
