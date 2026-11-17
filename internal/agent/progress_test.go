package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/failureguard"
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
