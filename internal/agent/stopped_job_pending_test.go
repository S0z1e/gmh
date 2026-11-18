package agent

import (
	"context"
	"errors"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoppedJobNextBatchWaitsForStoppedOutcome(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.BackgroundThreshold = 150 * time.Millisecond
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "launch" {
			<-gate
		}
	}
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "retry-next-step" {
			release()
			<-c.Exec.Jobs()[0].Done()
		}
	}
	args := `{"command":"if test -f ready; then printf retry; : > later; exit 0; fi; printf initial; : > ready; exec sleep 30"}`
	_, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop)}, nil, true)
	}, func() string { return toolCallSSE("retry-next-step", "bash", args) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Exec.Root, "later")); !os.IsNotExist(err) {
		t.Fatalf("next-batch retry executed before stopped outcome was available: %v", err)
	}
}

func TestStoppedJobPendingAnchorSurvivesLaterStop(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.Exec.BackgroundThreshold = 150 * time.Millisecond
	args := `{"command":"if test -f ready; then printf 'retry-%s' repaired; : > retried; exit 0; fi; printf 'original-%s' failed; : > ready; exec sleep 30"}`
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "original" {
			<-gate
		}
	}
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "stop-a" && e.ToolStatus != tools.ResultDeferred {
			t.Errorf("first stop not pending: %s", e.ToolStatus)
		}
		if e.Kind == EventToolEnd && e.ToolCallID == "stop-b" {
			release()
			<-c.Exec.Jobs()[0].Done()
		}
	}
	reqs, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("original", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		j, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: `: > b-ready; exec sleep 30`, Background: true})
		if err != nil || j.ID != "bg-2" {
			t.Fatalf("second job %v: %v", j, err)
		}
		waitStoppedJobFile(t, c, "b-ready")
		return toolCallSSE("stop-a", "bash", stoppedJobStop)
	}, func() string { return toolCallSSE("retry", "bash", args) }, func() string {
		if _, err := os.Stat(filepath.Join(c.Exec.Root, "retried")); !os.IsNotExist(err) {
			t.Fatal("retry executed before the original outcome", err)
		}
		return toolCallSSE("stop-b", "bash", `{"killJobId":"bg-2"}`)
	}, func() string {
		return budgetSSE("", []llm.ToolCall{budgetCall("stop-b-after-save", "bash", `{"killJobId":"bg-2"}`), budgetCall("retry-after-save", "bash", args)}, nil, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var anchorA, anchorB, actual string
	for _, m := range h {
		if m.Role == "tool" && m.ToolCallID == "stop-a" {
			anchorA = m.InvocationID
		}
		if m.Role == "tool" && m.ToolCallID == "stop-b" {
			anchorB = m.InvocationID
		}
		if m.Kind == "job" && strings.Contains(m.Content, "original-failed") {
			actual = m.Observation.BeforeInvocationID
		}
	}
	if actual != anchorA {
		t.Errorf("original completion anchored to %q; want first stop %q, sibling stop is %q", actual, anchorA, anchorB)
	}
	state, _, _, err := c.compactState(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range state.Failures {
		if f.Fingerprint == factDigest(CanonicalToolKey("bash", args)) {
			t.Error("successful retry left stale original failure")
		}
	}
	if len(reqs) < 6 {
		t.Fatalf("missing sixth request, got %d", len(reqs))
	}
	i, j := strings.Index(reqs[5], "original-failed"), strings.Index(reqs[5], "retry-repaired")
	if i < 0 || j < 0 || i > j {
		t.Errorf("wire order original=%d retry=%d", i, j)
	}
}

func TestStoppedJobPreviewUsesUnusedHeadSpace(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = t.TempDir()
	reqs, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`for n in {1..30}; do printf 'x\n'; done; printf 'CRITICAL-%s\n' DIAG; for n in {1..29}; do printf '%0850d\n' "$n"; done; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) < 3 || !strings.Contains(reqs[2], "CRITICAL-DIAG") {
		t.Fatal("30 tiny head lines and 25KiB captured tail fit together, but diagnostic at start of tail is missing")
	}
}

func TestStoppedJobGroupUsesShortSiblingSpace(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = ""
	c.OnEvent = func(e Event) {
		if e.Kind != EventToolStart || e.ToolCallID != "stop" {
			return
		}
		j, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: `printf 'short-%s' sibling`, Background: true})
		if err != nil {
			t.Fatal(err)
		}
		<-j.Done()
	}
	reqs, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%013000d\n' 0; printf 'CRITICAL-%s\n' FROMMIDDLE; printf '%013000d\n' 0; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) < 3 || !strings.Contains(reqs[2], "short-sibling") {
		t.Fatal("missing sibling")
	}
	if !strings.Contains(reqs[2], "CRITICAL-FROMMIDDLE") {
		t.Fatal("26KiB + tiny sibling fit together, but diagnostic in the middle is missing")
	}
}

func TestStoppedJobDrainedResultPrecedesRetry(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.Exec.BackgroundThreshold = 150 * time.Millisecond
	c.DrainTimeout = 20 * time.Millisecond
	args := `{"command":"if test -f ready; then printf 'retry-%s' repaired; exit 0; fi; printf 'drained-%s' original; : > ready; exec sleep 30"}`
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "original" {
			<-gate
		}
	}
	recorded := func() bool {
		h, err := session.Load(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range h {
			if m.Kind == "job" && strings.Contains(m.Content, "drained-original") {
				return true
			}
		}
		return false
	}
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolStart && e.ToolCallID == "retry" && !recorded() {
			t.Error("retry starts after target finished but before its result is saved")
		}
	}
	_, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("original", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("stop-a", "bash", stoppedJobStop)
	}, func() string { return textSSE("waiting") }, func() string {
		j := c.Exec.Jobs()[0]
		if !j.Reported() {
			t.Fatal("did not drain")
		}
		release()
		<-j.Done()
		return toolCallSSE("retry", "bash", args)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recorded() {
		t.Error("turn ended without the already completed stop outcome")
	}
}

func TestStoppedJobPendingCompletionEventFollowsSave(t *testing.T) {
	c, s := stoppedJobSession(t)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "original" {
			<-gate
		}
	}
	seen := 0
	c.OnEvent = func(e Event) {
		if e.Kind != EventToolBackgroundDone || e.ToolCallID != "original" {
			return
		}
		seen++
		if seen > 1 {
			return
		}
		h, err := session.Load(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		saved := false
		for _, m := range h {
			if m.Kind == "job" && m.InvocationID == e.InvocationID {
				saved = true
			}
		}
		if !saved {
			t.Fatal("completion event preceded its durable outcome")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("original", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`)) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("stop-a", "bash", stoppedJobStop)
	}, func() string {
		release()
		<-c.Exec.Jobs()[0].Done()
		return toolCallSSE("next", "bash", `{"command":":"}`)
	})
	if !errors.Is(err, os.ErrClosed) || !c.Exec.Jobs()[0].Delivered() {
		t.Fatalf("want acknowledged job then subsequent write error: %v", err)
	}
	c.Session, err = session.Open(s.ID, session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Session.Close()
	if _, err := runStoppedJobSteps(t, c); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("one completion was published %d times", seen)
	}
}
