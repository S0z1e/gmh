package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobDeferredStopDoesNotRecordFailure(t *testing.T) {
	c, s := stoppedJobSession(t)
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
		if e.Kind == EventToolEnd && e.ToolCallID == "stop" {
			release()
			<-c.Exec.Jobs()[0].Done()
		}
	}
	_, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'initial-%s' success; : > ready`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := c.compactState(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Failures) > 0 {
		t.Errorf("a successful job and deferred stop left unresolved failures: %+v", state.Failures)
	}
}

func TestStoppedJobDeferredCompletionPrecedesNextBatch(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.Exec.BackgroundThreshold = 150 * time.Millisecond
	args := `{"command":"if test -f ready; then printf 'retry-%s' output; : > later; exit 0; fi; printf 'initial-%s' payload; : > ready; exec sleep 30"}`
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
		if e.Kind == EventToolStart && e.ToolCallID == "retry-next-step" {
			h, err := session.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range h {
				if m.Kind == "job" && strings.Contains(m.Content, "initial-payload") {
					return
				}
			}
			t.Error("next-batch tool started before an already completed stopped job was saved")
		}
	}
	req, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("same-batch", "bash", args)}, nil, true)
	}, func() string {
		if _, err := os.Stat(filepath.Join(c.Exec.Root, "later")); !os.IsNotExist(err) {
			t.Errorf("same-batch tool was not deferred: %v", err)
		}
		release()
		<-c.Exec.Jobs()[0].Done()
		return toolCallSSE("retry-next-step", "bash", args)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(req) < 4 {
		t.Fatalf("expected another request, got %d", len(req))
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.promptView(h) {
		if m.Kind == "job" && strings.Contains(m.Content, "initial-payload") {
			t.Error("stopped job still unanchored in cold replay")
		}
	}
	initial, retry := strings.Index(req[3], "initial-payload"), strings.Index(req[3], "retry-output")
	if initial < 0 || retry < 0 || initial > retry {
		t.Errorf("later provider request orders stopped job after retry: original=%d retry=%d", initial, retry)
	}
}

func TestStoppedJobPreviewKeepsShortOutput(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = ""
	req, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%03500d\n' 0; printf 'critical-%s\n' diagnostic; printf '%03500d\n' 0; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	if len(req) < 3 || !strings.Contains(req[2], "critical-diagnostic") {
		t.Errorf("stopped-job display omitted text from a 7 KB result that fits the 32 KiB cap and has no output file")
	}
}

func TestStoppedJobSiblingCompletionPrecedesRetry(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.Exec.BackgroundThreshold = 150 * time.Millisecond
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "sibling-launch" {
			<-gate
		}
	}
	args := `{"command":"if test -f sibling-ready; then printf 'retry-%s' output; exit 0; fi; printf 'sibling-%s' failure; : > sibling-ready; exit 2"}`
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "stop" {
			release()
			<-c.Exec.Jobs()[1].Done()
		}
		if e.Kind == EventToolStart && e.ToolCallID == "later" {
			h, err := session.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range h {
				if m.Kind == "job" && strings.Contains(m.Content, "sibling-failure") {
					return
				}
			}
			t.Error("later tool started before the already finished sibling outcome was durable")
		}
	}
	req, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`)) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("sibling-launch", "bash", args)
	}, func() string {
		waitStoppedJobFile(t, c, "sibling-ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("later", "bash", args)}, nil, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := c.compactState(h)
	if err != nil {
		t.Fatal(err)
	}
	want := factDigest(CanonicalToolKey("bash", args))
	for _, f := range state.Failures {
		if f.Fingerprint == want {
			t.Errorf("checkpoint kept the old sibling failure despite successful retry: %+v", f)
		}
	}
	if len(req) < 4 {
		t.Fatalf("expected another request, got %d", len(req))
	}
	if strings.Index(req[3], "sibling-failure") > strings.Index(req[3], "retry-output") {
		t.Error("provider saw the sibling failure after the successful retry")
	}
}

func TestStoppedJobPreviewNormalizesBeforeCut(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = t.TempDir()
	req, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'ACTUAL-%s' HEAD; printf '%03000d\n%03000d' 0 0 | LC_ALL=C tr 0 '\200'; printf 'ACTUAL-%s' TAIL; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	if len(req) < 3 || !strings.Contains(req[2], "ACTUAL-HEAD") || !strings.Contains(req[2], "ACTUAL-TAIL") {
		t.Errorf("binary clip dropped an output endpoint; head=%v tail=%v", strings.Contains(req[2], "ACTUAL-HEAD"), strings.Contains(req[2], "ACTUAL-TAIL"))
	}
}

func TestStoppedJobDeferredSaveFailureStopsBeforeGrace(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.MaxSteps = 2
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
		if e.Kind == EventToolEnd && e.ToolCallID == "skipped" {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			release()
			<-c.Exec.Jobs()[0].Done()
		}
	}
	req, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'undurable-%s' output; : > ready; exec sleep 30`))
	}, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("skipped", "bash", `{"command":": > skipped"}`)}, nil, true)
	})
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("want save error, got %v", err)
	}
	if len(req) != 2 {
		t.Errorf("save failure still ran grace provider request: %d calls", len(req))
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(req) > 2 {
		for _, m := range h {
			if m.ToolCallID == "skipped" && m.Kind == "interrupted" {
				t.Log("cold replay calls it interrupted, live grace calls it not executed")
			}
		}
	}
}

func TestStoppedJobApprovalDenialKeepsOriginalFailure(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.Mode = "suggest"
	c.Ask = func(r ApprovalRequest) (bool, error) { return r.ToolCallID != "retry-denied", nil }
	args := stoppedJobLaunch(`printf 'real-%s' failure; : > ready; exec sleep 30`)
	_, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("retry-denied", "bash", args)}, nil, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := c.compactState(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range state.Failures {
		if f.Fingerprint == factDigest(CanonicalToolKey("bash", args)) && strings.Contains(f.Output.Head, "real-failure") {
			return
		}
	}
	t.Errorf("checkpoint discarded original stopped-job failure: %+v", state.Failures)
}
