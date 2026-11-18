package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobReportedStopAtStepCap(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.MaxSteps = 4
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
	waited := false
	c.OnEvent = func(e Event) {
		if e.Kind == EventBackgroundWait && e.Text == "bg-1" && c.Exec.Jobs()[0].Reported() {
			waited = true
			release()
			<-c.Exec.Jobs()[0].Done()
		}
	}
	reqs, err := runStoppedJobSteps(t, c,
		func() string {
			return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf 'cap-%s' result; : > ready; exec sleep 30`))
		},
		func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) },
		func() string { return textSSE("drain") },
		func() string { return toolCallSSE("deferred", "bash", `{"command":"true"}`) },
		func() string { return textSSE("summary") },
	)
	if !waited || err == nil || c.LastTerminalReason != TerminalMaxTurns || len(reqs) != 5 {
		t.Fatalf("waited=%v reqs=%d reason=%s err=%v", waited, len(reqs), c.LastTerminalReason, err)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved := false
	for _, m := range h {
		saved = saved || (m.Kind == "job" && strings.Contains(m.Content, "cap-result"))
	}
	if !saved || !c.Exec.Jobs()[0].Delivered() || !strings.Contains(reqs[4], "cap-result") {
		t.Errorf("reported stop completed before cap but saved=%v delivered=%v seenByGrace=%v", saved, c.Exec.Jobs()[0].Delivered(), strings.Contains(reqs[4], "cap-result"))
	}
}

func TestStoppedJobFailureCapWaitsForInFlightRepair(t *testing.T) {
	for _, beforeRequest := range []bool{false, true} {
		name := "after-answer"
		if beforeRequest {
			name = "before-request"
		}
		t.Run(name, func(t *testing.T) {
			c, s := stoppedJobSession(t)
			c.DrainTimeout = 20 * time.Millisecond
			failGate, repairGate := make(chan struct{}), make(chan struct{})
			var failOnce, repairOnce sync.Once
			releaseFail := func() { failOnce.Do(func() { close(failGate) }) }
			releaseRepair := func() { repairOnce.Do(func() { close(repairGate) }) }
			defer releaseFail()
			defer releaseRepair()
			c.Exec.Audit = func(e tools.AuditEntry) {
				if e.Kind != "bash" {
					return
				}
				switch e.ToolCallID {
				case "first":
					<-failGate
				case "repair":
					<-repairGate
				}
			}
			drained := false
			c.OnEvent = func(e Event) {
				if e.Kind == EventToolEnd && e.ToolCallID == "first" {
					waitStoppedJobFile(t, c, "claim")
				}
				if beforeRequest && e.Kind == EventToolEnd && e.ToolCallID == "repair" {
					releaseFail()
					<-c.Exec.Jobs()[0].Done()
				}
				if e.Kind == EventBackgroundWait {
					drained = true
					releaseRepair()
					<-c.Exec.Jobs()[1].Done()
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
				func() string {
					if !beforeRequest {
						releaseFail()
						<-c.Exec.Jobs()[0].Done()
					}
					return textSSE("wait")
				},
				func() string { return textSSE("wait") },
				func() string { return textSSE("done") },
			)
			h, loadErr := session.Load(s.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			saved := false
			for _, m := range h {
				saved = saved || (m.Kind == "job" && strings.Contains(m.Content, "repair-done"))
			}
			if err != nil || c.LastTerminalReason != TerminalEndTurn || !drained || !saved || !strings.Contains(reqs[len(reqs)-1], "repair-done") {
				t.Errorf("reason=%s requests=%d drained=%v saved=%v err=%v", c.LastTerminalReason, len(reqs), drained, saved, err)
			}
		})
	}
}
