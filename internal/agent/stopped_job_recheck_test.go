package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobCancelAfterCompletionOnLastStep(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.MaxSteps = 4
	c.DrainTimeout = 20 * time.Millisecond
	release := stoppedJobAuditGate(c, t, "launch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var requests int
	c.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		body := textSSE("grace")
		switch requests {
		case 1:
			body = toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'cap-%s' result; : > ready; exec sleep 30`))
		case 2:
			waitStoppedJobFile(t, c, "ready")
			body = toolCallSSE("stop", "bash", stoppedJobStop)
		case 3:
			body = textSSE("drain")
		case 4:
			body = toolCallSSE("deferred", "bash", `{"command":"true"}`)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	canceled := false
	c.OnEvent = func(e Event) {
		if e.Kind == EventBackgroundWait && e.Text == "bg-1" && c.Exec.Jobs()[0].Reported() {
			release()
			<-c.Exec.Jobs()[0].Done()
		}
		if e.Kind == EventToolBackgroundDone {
			canceled = true
			cancel()
		}
	}
	_, _, err := c.Run(ctx, []llm.Message{{Role: "user", Content: "work"}})
	h, loadErr := session.Load(s.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	saved := false
	for _, m := range h {
		saved = saved || (m.Kind == "job" && strings.Contains(m.Content, "cap-result"))
	}
	if !saved || !canceled || requests != 4 || !errors.Is(err, context.Canceled) || c.LastTerminalReason != TerminalAbort {
		t.Fatalf("saved=%v canceled=%v requests=%d reason=%s err=%v", saved, canceled, requests, c.LastTerminalReason, err)
	}
}

func TestStoppedJobReadyReportedRepairAtRecheck(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.DrainTimeout = 20 * time.Millisecond
	failGate, repairGate, freshGate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		if failGate != nil {
			close(failGate)
		}
		if repairGate != nil {
			close(repairGate)
		}
		if freshGate != nil {
			close(freshGate)
		}
	})
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind != "bash" {
			return
		}
		switch e.ToolCallID {
		case "first":
			<-failGate
		case "repair":
			<-repairGate
		case "fresh":
			<-freshGate
		}
	}
	drained := false
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "first" {
			waitStoppedJobFile(t, c, "claim")
		}
		if e.Kind == EventBackgroundWait && e.Text == "bg-3" {
			drained = true
			close(repairGate)
			<-c.Exec.Jobs()[1].Done()
			repairGate = nil
			close(freshGate)
			<-c.Exec.Jobs()[2].Done()
			freshGate = nil
		}
	}
	args := stoppedJobLaunch(`if mkdir claim 2>/dev/null; then exit 17; fi; printf 'repair-%s' done`)
	reqs, err := runStoppedJobSteps(t, c,
		func() string {
			return budgetSSE("", []llm.ToolCall{
				budgetCall("fail-a", "bash", `{"command":"exit 12"}`),
				budgetCall("fail-b", "bash", `{"command":"exit 13"}`),
				budgetCall("first", "bash", args),
				budgetCall("repair", "bash", args),
			}, nil, true)
		},
		func() string { return textSSE("initial drain") },
		func() string {
			if !c.Exec.Jobs()[1].Reported() {
				t.Fatal("repair not initially reported")
			}
			return toolCallSSE("fresh", "bash", stoppedJobLaunch(`printf 'fresh-%s' done`))
		},
		func() string {
			close(failGate)
			<-c.Exec.Jobs()[0].Done()
			failGate = nil
			return textSSE("looks done")
		},
		func() string { return textSSE("final") },
	)
	h, loadErr := session.Load(s.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	saved := false
	repairIndex, freshIndex := -1, -1
	for i, m := range h {
		saved = saved || (m.Kind == "job" && strings.Contains(m.Content, "repair-done"))
		if m.Kind == "job" && strings.Contains(m.Content, "repair-done") {
			repairIndex = i
		}
		if m.Kind == "job" && strings.Contains(m.Content, "fresh-done") {
			freshIndex = i
		}
	}
	if repairIndex < 0 || freshIndex < 0 || repairIndex >= freshIndex {
		t.Errorf("earlier repair saved out of completion order: repair=%d fresh=%d", repairIndex, freshIndex)
	}
	if !drained || !saved || err != nil || c.LastTerminalReason != TerminalEndTurn {
		t.Fatalf("drained=%v saved=%v requests=%d reason=%s err=%v", drained, saved, len(reqs), c.LastTerminalReason, err)
	}
}
