package agent

import (
	"errors"
	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/turnbudget"
	"slices"
	"strings"
	"testing"
)

func TestStoppedJobBudgetRejectKeepsOriginalFailure(t *testing.T) {
	c, s := stoppedJobSession(t)
	seconds := int64(1)
	c.TurnBudget = &turnbudget.Policy{MaxDurationSeconds: &seconds}
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "stop" {
			<-c.progress.ctx.Done()
		}
	}
	args := stoppedJobLaunch(`printf 'stopped-%s' original; : > ready; exec sleep 30`)
	_, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("retry", "bash", args)}, nil, true)
	})
	var exhausted *turnbudget.Exceeded
	if !errors.As(err, &exhausted) {
		t.Fatalf("want duration exhaustion, got %v", err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := (&Config{}).compactState(h)
	if err != nil {
		t.Fatal(err)
	}
	key := factDigest(CanonicalToolKey("bash", args))
	if !slices.ContainsFunc(state.Failures, func(f checkpoint.Failure) bool {
		return f.Fingerprint == key && strings.Contains(f.Output.Head, "stopped-original")
	}) {
		t.Fatalf("budget rejection replaced original failure: %+v", state.Failures)
	}
}
