package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobShortSiblingRetainsMetadata(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = t.TempDir()
	var path string
	c.OnEvent = func(e Event) {
		if e.Kind != EventToolStart || e.ToolCallID != "stop" {
			return
		}
		j, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: `printf 'small-%s' sibling; exit 17`, Background: true})
		if err != nil {
			t.Fatal(err)
		}
		<-j.Done()
		path = j.OutputFile
	}
	reqs, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(reqs[2]), &raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range raw.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "small-sibling") {
			if !strings.Contains(m.Content, "(exit 17)") || !strings.Contains(m.Content, path) {
				t.Errorf("short sibling lost exit code or file: %q", m.Content)
			}
			return
		}
	}
	t.Fatal("missing sibling")
}

func TestReportedBackgroundJobDoesNotDiscardFinalAnswer(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.MaxSteps = 4
	c.DrainTimeout = 15 * time.Millisecond
	reqs, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return textSSE("drain") }, func() string {
		if !c.Exec.Jobs()[0].Reported() {
			t.Fatal("not drained")
		}
		return toolCallSSE("check", "bash", `{"command":"test -f ready"}`)
	}, func() string { return textSSE("final answer") })
	if err != nil || len(reqs) != 4 {
		t.Errorf("final answer should end step four, got %d requests and %v", len(reqs), err)
	}
}

func TestStoppedJobDrainedCompletionDuringFinalResponse(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.DrainTimeout = 20 * time.Millisecond
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "launch" {
			<-gate
		}
	}
	done := 0
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolBackgroundDone {
			done++
		}
	}
	reqs, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'terminal-%s' result; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) }, func() string { return textSSE("wait") }, func() string {
		j := c.Exec.Jobs()[0]
		if !j.Reported() {
			t.Fatal("not drained")
		}
		release()
		<-j.Done()
		return textSSE("final")
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved := false
	for _, m := range h {
		if m.Kind == "job" && strings.Contains(m.Content, "terminal-result") {
			saved = true
		}
	}
	if !saved || done != 1 || len(reqs) < 5 || !strings.Contains(reqs[4], "terminal-result") {
		t.Errorf("finalized stop missed before end: saved=%v done=%d requests=%d", saved, done, len(reqs))
	}
}

func TestStoppedJobBarrierWaitsBeforeRequestingRetry(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.MaxSteps = 6
	c.DrainTimeout = 2 * time.Second
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
			time.AfterFunc(300*time.Millisecond, release)
		}
	}
	attempts := 0
	retry := func() string {
		if _, err := os.Stat(filepath.Join(c.Exec.Root, "retried")); err == nil {
			return textSSE("done")
		}
		attempts++
		return toolCallSSE("retry-"+string(rune('0'+attempts)), "bash", `{"command":"printf 'retry-%s' did-run; : > retried"}`)
	}
	reqs, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'old-%s' stopped; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) }, retry, retry, retry, retry)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(reqs) != 5 || !strings.Contains(reqs[4], "retry-did-run") {
		t.Fatalf("did not wait for finalization before reconsidering retry: attempts=%d requests=%d", attempts, len(reqs))
	}
}

func TestStoppedJobBarrierTimeoutEndsIncomplete(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.DrainTimeout = 20 * time.Millisecond
	gate := make(chan struct{})
	defer close(gate)
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "launch" {
			<-gate
		}
	}
	reqs, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) }, func() string {
		return toolCallSSE("retry", "bash", `{"command":": > unexpected"}`)
	})
	if err == nil || !strings.Contains(err.Error(), "still finalizing") || len(reqs) != 3 || c.LastTaskOutcome != OutcomeIncomplete {
		t.Fatalf("timeout did not end incomplete: requests=%d outcome=%s err=%v", len(reqs), c.LastTaskOutcome, err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range h {
		if m.Kind == "task_outcome" && strings.Contains(m.Content, "still finalizing") {
			return
		}
	}
	t.Fatal("missing durable incomplete outcome")
}
