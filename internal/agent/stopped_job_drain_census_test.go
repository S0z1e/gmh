package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobOrdinaryDrainEmitsAbort(t *testing.T) {
	for name, maxSteps := range map[string]int{"ordinary": 0, "step-cap": 2} {
		t.Run(name, func(t *testing.T) {
			c, s := stoppedJobSession(t)
			c.MaxSteps = maxSteps
			expectedRequests := 3
			if maxSteps > 0 {
				expectedRequests = maxSteps
			}
			c.DrainTimeout = 200 * time.Millisecond
			release := stoppedJobAuditGate(c, t, "launch")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			requests := 0
			c.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				requests++
				var body string
				switch requests {
				case 1:
					body = toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'ordinary-%s' saved; : > ready; exec sleep 30`))
				case 2:
					waitStoppedJobFile(t, c, "ready")
					body = toolCallSSE("stop", "bash", stoppedJobStop)
				case 3:
					body = textSSE("wait")
				default:
					return nil, errors.New("unexpected request")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			outcomes := []TerminalReason{}
			c.OnEvent = func(e Event) {
				switch e.Kind {
				case EventBackgroundWait:
					release()
					select {
					case <-c.Exec.Jobs()[0].Done():
					case <-time.After(time.Second):
						t.Fatal("test job didn't finish")
					}
				case EventToolBackgroundDone:
					cancel()
				case EventError, EventDone:
					outcomes = append(outcomes, e.TerminalReason)
				}
			}
			_, _, err := c.Run(ctx, []llm.Message{{Role: "user", Content: "work"}})
			h, loadErr := session.Load(s.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			saved := 0
			for _, m := range h {
				if m.Kind == "job" && strings.Contains(m.Content, "ordinary-saved") {
					saved++
				}
			}
			if saved != 1 || requests != expectedRequests || !errors.Is(err, context.Canceled) || c.LastTerminalReason != TerminalAbort || len(outcomes) != 1 || outcomes[0] != TerminalAbort {
				t.Fatalf("saved=%d requests=%d reason=%q terminal events=%q err=%v", saved, requests, c.LastTerminalReason, outcomes, err)
			}
		})
	}
}

func TestStoppedJobFreshCompletionDuringTimeoutCensus(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.MaxSteps = 3
	c.DrainTimeout = 30 * time.Millisecond
	older, fresh := make(chan struct{}), make(chan struct{})
	var oldOnce, freshOnce sync.Once
	releaseOlder := func() { oldOnce.Do(func() { close(older) }) }
	releaseFresh := func() { freshOnce.Do(func() { close(fresh) }) }
	defer releaseOlder()
	defer releaseFresh()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" {
			switch e.ToolCallID {
			case "old":
				<-older
			case "fresh":
				<-fresh
			}
		}
	}
	wait := func(j *tools.Job) {
		select {
		case <-j.Done():
		case <-time.After(time.Second):
			t.Fatalf("job %s didn't finish", j.ID)
		}
	}
	c.OnEvent = func(e Event) {
		if e.Kind == EventBackgroundWait && e.Text == "bg-2" {
			releaseOlder()
			wait(c.Exec.Jobs()[0])
		}
		if e.Kind == EventToolBackgroundDone && e.JobID == "bg-1" {
			releaseFresh()
			wait(c.Exec.Jobs()[1])
		}
	}
	reqs, err := runStoppedJobSteps(t, c,
		func() string { return toolCallSSE("old", "bash", stoppedJobLaunch(`printf 'older-%s' finished`)) },
		func() string { return textSSE("wait once") },
		func() string {
			if !c.Exec.Jobs()[0].Reported() {
				t.Fatal("older job not reported")
			}
			return toolCallSSE("fresh", "bash", stoppedJobLaunch(`printf 'fresh-%s' finished`))
		},
		func() string { return textSSE("summary") },
	)
	if len(reqs) != 4 || c.LastTerminalReason != TerminalMaxTurns {
		t.Fatalf("len=%d reason=%s err=%v", len(reqs), c.LastTerminalReason, err)
	}
	h, loadErr := session.Load(s.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	j := c.Exec.Jobs()[1]
	saved := 0
	for _, m := range h {
		if m.Kind == "job" && strings.Contains(m.Content, "fresh-finished") {
			saved++
		}
	}
	if _, done := j.Result(); done {
		if saved != 1 || !strings.Contains(reqs[3], "fresh-finished") {
			t.Fatalf("fresh finished during census: saved=%d onWire=%v reported=%v delivered=%v", saved, strings.Contains(reqs[3], "fresh-finished"), j.Reported(), j.Delivered())
		}
	} else if !j.Reported() {
		t.Fatal("fresh is neither delivered nor reported")
	}
}
