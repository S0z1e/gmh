package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func stoppedJobAuditGate(c *Config, t *testing.T, launch string) func() {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == launch {
			<-gate
		}
	}
	return release
}

func TestStoppedJobFinalResponseLeavesUserTurnAnthropic(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint("stopped=", stopped), func(t *testing.T) {
			testBackgroundFinalResponseAnthropic(t, stopped)
		})
	}
}

func testBackgroundFinalResponseAnthropic(t *testing.T, stopped bool) {
	c, _ := stoppedJobSession(t)
	c.DrainTimeout = 20 * time.Millisecond
	release := stoppedJobAuditGate(c, t, "launch")
	anthropic := func(block map[string]any, stop string) string {
		events := []any{
			map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "role": "assistant", "usage": map[string]int{"input_tokens": 1}}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": block},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": stop}, "usage": map[string]int{"output_tokens": 1}},
			map[string]any{"type": "message_stop"},
		}
		var s strings.Builder
		for _, e := range events {
			b, _ := json.Marshal(e)
			fmt.Fprintf(&s, "data: %s\n\n", b)
		}
		return s.String()
	}
	call := func(id, arg string) string {
		return anthropic(map[string]any{"type": "tool_use", "id": id, "name": "bash", "input": json.RawMessage(arg)}, "tool_use")
	}
	text := func(text string) string { return anthropic(map[string]any{"type": "text", "text": text}, "end_turn") }
	requests := 0
	c.Client = &llm.Client{API: "anthropic", Model: "claude-sonnet-4-6", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		raw, _ := io.ReadAll(r.Body)
		var parsed struct{ Messages []struct{ Role string } }
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		response := func(status int, payload string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
		}
		if parsed.Messages[len(parsed.Messages)-1].Role != "user" {
			return response(400, `{"type":"error","error":{"type":"invalid_request_error","message":"This model does not support assistant message prefill"}}`)
		}
		if !stopped {
			switch requests {
			case 1:
				return response(200, call("launch", stoppedJobLaunch(`printf 'terminal-%s' result; : > ready; exit 0`)))
			case 2:
				waitStoppedJobFile(t, c, "ready")
				release()
				<-c.Exec.Jobs()[0].Done()
				return response(200, text("final without evidence"))
			default:
				if !strings.Contains(string(raw), "terminal-result") {
					t.Error("evidence missing")
				}
				return response(200, text("revised final"))
			}
		}
		switch requests {
		case 1:
			return response(200, call("launch", stoppedJobLaunch(`printf 'terminal-%s' result; : > ready; exec sleep 30`)))
		case 2:
			waitStoppedJobFile(t, c, "ready")
			return response(200, call("stop", stoppedJobStop))
		case 3:
			return response(200, text("waiting"))
		case 4:
			j := c.Exec.Jobs()[0]
			if !j.Reported() {
				t.Fatal("not reported")
			}
			release()
			<-j.Done()
			return response(200, text("final without evidence"))
		default:
			if !strings.Contains(string(raw), "terminal-result") {
				t.Error("evidence missing")
			}
			return response(200, text("revised final"))
		}
	})}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, _, err := c.Run(ctx, []llm.Message{{Role: "user", Content: "work"}})
	if err != nil || final != "revised final" {
		t.Fatalf("requests=%d final=%q err=%v", requests, final, err)
	}
}

func TestStoppedJobFinalResponseRespectsFailureCap(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.DrainTimeout = 20 * time.Millisecond
	release := stoppedJobAuditGate(c, t, "launch")
	reqs, err := runStoppedJobSteps(t, c,
		func() string {
			return budgetSSE("", []llm.ToolCall{
				budgetCall("fail-1", "bash", `{"command":"exit 12"}`),
				budgetCall("fail-2", "bash", `{"command":"exit 13"}`),
				budgetCall("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`)),
			}, nil, true)
		},
		func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) },
		func() string { release(); <-c.Exec.Jobs()[0].Done(); return textSSE("looks done") },
		func() string {
			if c.progress.failures != MaxFailedFixAttempts {
				t.Fatalf("failures=%d", c.progress.failures)
			}
			return toolCallSSE("unexpected", "bash", `{"command":": > unexpected"}`)
		},
	)
	if _, statErr := os.Stat(filepath.Join(c.Exec.Root, "unexpected")); !os.IsNotExist(statErr) {
		t.Errorf("command executed after failure cap: %v", statErr)
	}
	if len(reqs) < 4 {
		t.Fatalf("expected grace request, requests=%d err=%v", len(reqs), err)
	}
	var wire struct{ Tools []any }
	if jsonErr := json.Unmarshal([]byte(reqs[3]), &wire); jsonErr != nil {
		t.Fatal(jsonErr)
	}
	if len(wire.Tools) != 0 {
		t.Errorf("request after failure cap exposed %d tools", len(wire.Tools))
	}
	if c.LastTerminalReason != TerminalBlocked {
		t.Errorf("terminal=%s err=%v", c.LastTerminalReason, err)
	}
}

func TestStoppedJobWaitCollectsReportedSibling(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.DrainTimeout = 20 * time.Millisecond
	gateA, gateB := make(chan struct{}), make(chan struct{})
	var onceA, onceB sync.Once
	releaseA := func() { onceA.Do(func() { close(gateA) }) }
	releaseB := func() { onceB.Do(func() { close(gateB) }) }
	defer releaseA()
	defer releaseB()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind != "bash" {
			return
		}
		switch e.ToolCallID {
		case "launch":
			<-gateA
		case "sibling":
			<-gateB
		}
	}
	c.OnEvent = func(e Event) {
		if e.Kind != EventToolEnd || e.ToolCallID != "deferred" {
			return
		}
		jobs := c.Exec.Jobs()
		if !jobs[0].Reported() || !jobs[1].Reported() {
			t.Fatal("jobs not reported")
		}
		releaseB()
		<-jobs[1].Done()
		releaseA()
		<-jobs[0].Done()
	}
	reqs, err := runStoppedJobSteps(t, c,
		func() string {
			return budgetSSE("", []llm.ToolCall{
				budgetCall("launch", "bash", stoppedJobLaunch(`printf 'stopped-%s' target; : > ready; exec sleep 30`)),
				budgetCall("sibling", "bash", stoppedJobLaunch(`printf 'sibling-%s' evidence; : > sibling-ready; exit 17`)),
			}, nil, true)
		},
		func() string {
			waitStoppedJobFile(t, c, "ready")
			waitStoppedJobFile(t, c, "sibling-ready")
			return toolCallSSE("stop", "bash", stoppedJobStop)
		},
		func() string { return textSSE("drain") },
		func() string { return toolCallSSE("deferred", "bash", `{"command":"true"}`) },
		func() string { return textSSE("answer") },
	)
	if err != nil || len(reqs) < 5 {
		t.Fatalf("requests=%d err=%v", len(reqs), err)
	}
	if !strings.Contains(reqs[4], "stopped-target") || !strings.Contains(reqs[4], "sibling-evidence") {
		t.Errorf("missing original evidence from retry prompt: target=%v sibling=%v", strings.Contains(reqs[4], "stopped-target"), strings.Contains(reqs[4], "sibling-evidence"))
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, second := -1, -1
	for i, m := range h {
		if m.Kind != "job" {
			continue
		}
		if strings.Contains(m.Content, "sibling-evidence") {
			first = i
		}
		if strings.Contains(m.Content, "stopped-target") {
			second = i
		}
	}
	if first < 0 || second < 0 || first >= second {
		t.Errorf("earlier completion saved after later completion: sibling=%d target=%d", first, second)
	}
}

func TestStoppedJobWaitTimeoutSavesReadySibling(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.DrainTimeout = 20 * time.Millisecond
	gateA, gateB := make(chan struct{}), make(chan struct{})
	var onceB sync.Once
	releaseB := func() { onceB.Do(func() { close(gateB) }) }
	defer close(gateA)
	defer releaseB()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind != "bash" {
			return
		}
		switch e.ToolCallID {
		case "launch":
			<-gateA
		case "sibling":
			<-gateB
		}
	}
	c.OnEvent = func(e Event) {
		if e.Kind == EventBackgroundWait && e.Text == "bg-1" {
			j := c.Exec.Jobs()[1]
			releaseB()
			<-j.Done()
		}
	}
	reqs, err := runStoppedJobSteps(t, c,
		func() string {
			return budgetSSE("", []llm.ToolCall{
				budgetCall("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`)),
				budgetCall("sibling", "bash", stoppedJobLaunch(`printf 'sibling-%s' evidence; : > sibling-ready; exit 17`)),
			}, nil, true)
		},
		func() string {
			waitStoppedJobFile(t, c, "ready")
			waitStoppedJobFile(t, c, "sibling-ready")
			return toolCallSSE("stop", "bash", stoppedJobStop)
		},
		func() string { return textSSE("drain") },
		func() string { return toolCallSSE("deferred", "bash", `{"command":"true"}`) },
	)
	if err == nil || len(reqs) != 4 || c.LastTaskOutcome != OutcomeIncomplete {
		t.Fatalf("requests=%d outcome=%s err=%v", len(reqs), c.LastTaskOutcome, err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range h {
		if m.Kind == "job" && strings.Contains(m.Content, "sibling-evidence") {
			return
		}
	}
	t.Error("timeout returned with a ready sibling outcome unsaved")
}
