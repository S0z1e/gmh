package agent

import (
	"context"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
	"strings"
	"testing"
)

func TestQuotaAdmissionKeepsVerificationPass(t *testing.T) {
	c, s := stoppedJobSession(t)
	command := "printf checked"
	c.Verification = &verification.Policy{Required: []verification.Check{{Name: "test", Command: command}}}
	c.Exec.OutputDir = t.TempDir()
	c.Exec.OutputMaxBytes = int64(len("checked"))
	_, err := runStoppedJobSteps(t, c,
		func() string { return verificationWrite("input", "new work") },
		func() string { return verificationBash(command) },
		func() string { return verificationBash(command) },
	)
	h, loadErr := session.Load(s.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	var refused bool
	for _, m := range h {
		if m.Kind == notExecutedKind && strings.Contains(m.Content, "session bash output") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("quota retry wasn't refused")
	}
	state, _, _, compactErr := c.compactState(h)
	if compactErr != nil {
		t.Fatal(compactErr)
	}
	t.Logf("run error=%v outcome=%s latest=%+v compact verification=%+v", err, c.LastTaskOutcome, c.verifier.latest["test"], state.Verification)
	if len(state.Verification) != 1 || state.Verification[0].ExitCode == nil || *state.Verification[0].ExitCode != 0 {
		t.Fatal("refusal overwrote passing checkpoint verification")
	}
	if c.LastTaskOutcome != OutcomeCompleted {
		t.Fatal("non-executed retry replaced valid verification pass")
	}
}

func TestQuotaAdmissionKeepsVerificationFailure(t *testing.T) {
	c, s := stoppedJobSession(t)
	command := "printf actual-diagnostic; exit 2"
	c.Verification = &verification.Policy{Required: []verification.Check{{Name: "test", Command: command}}}
	c.Exec.OutputDir = t.TempDir()
	c.Exec.OutputMaxBytes = int64(len("actual-diagnostic"))
	_, _ = runStoppedJobSteps(t, c,
		func() string { return verificationWrite("input", "new work") },
		func() string { return toolCallSSE("first", "bash", `{"command":"printf actual-diagnostic; exit 2"}`) },
		func() string { return toolCallSSE("refused", "bash", `{"command":"printf actual-diagnostic; exit 2"}`) },
	)
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := c.compactState(h)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checkpoint verification=%+v", state.Verification)
	if len(state.Verification) != 1 || state.Verification[0].ExitCode == nil || *state.Verification[0].ExitCode != 2 {
		t.Fatal("refusal overwrote prior execution's verification exit code")
	}
}

func TestQuotaAdmissionLoopRemainsBounded(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = t.TempDir()
	c.Exec.OutputMaxBytes = 1
	if _, err := c.Exec.Bash(context.Background(), tools.BashArgs{Command: "printf x"}); err != nil {
		t.Fatal(err)
	}
	steps := []func() string{}
	for range 12 {
		steps = append(steps, func() string { return toolCallSSE("refused", "bash", `{"command":"printf y"}`) })
	}
	requests, err := runStoppedJobSteps(t, c, steps...)
	t.Logf("requests=%d error=%v terminal=%s", len(requests), err, c.LastTerminalReason)
	if err == nil || len(requests) > 10 {
		t.Fatal("unbounded refusals")
	}
}
