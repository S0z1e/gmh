package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobOversizedHookCannotBlockDelivery(t *testing.T) {
	c, _ := stoppedJobSession(t)
	hooks := &fakePluginRunner{result: map[string]string{}}
	c.Exec.Plugins = hooks
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolStart && e.ToolCallID == "stop" {
			hooks.result["bash"] = strings.Repeat("x", 4<<20)
		}
	}
	requests, err := runStoppedJobSteps(t, c,
		func() string {
			return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' stopped payload; : > ready; exec sleep 30`))
		},
		func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil || len(requests) < 3 || !strings.Contains(requests[2], "stopped-payload") {
		t.Fatalf("stop hook must not prevent delivery: requests=%d delivered=%v err=%v", len(requests), c.Exec.Jobs()[0].Delivered(), err)
	}
}

func TestStoppedJobAcknowledgementRequiresSavedOutcome(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolStart && e.ToolCallID == "stop" {
			j, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: ":", Background: true})
			if err != nil {
				t.Fatal(err)
			}
			<-j.Done()
		}
		if e.Kind == EventToolBackgroundDone && e.JobID == "bg-2" {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, runErr := runStoppedJobSteps(t, c,
		func() string {
			return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' stopped payload; : > ready; exec sleep 30`))
		},
		func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if !errors.Is(runErr, os.ErrClosed) {
		t.Fatalf("expected a save failure during partial delivery, got %v", runErr)
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var settled, outcome bool
	interrupted := false
	for _, m := range h {
		settled = settled || m.Role == "tool" && m.ToolCallID == "stop" && !m.ToolError
		interrupted = interrupted || m.ToolCallID == "stop" && m.Kind == "interrupted"
		if (m.Kind == "job" || m.ToolCallID == "stop") && strings.Contains(m.Content, "stopped-payload") {
			outcome = true
		}
	}
	if settled && !outcome {
		t.Fatalf("restart sees successful stop, but no target outcome was saved (delivered=%v)", c.Exec.Jobs()[0].Delivered())
	}
	if !interrupted {
		t.Fatal("missing explicit unknown outcome after partial delivery")
	}
}

func TestStoppedJobFinishesDuringStopEnd(t *testing.T) {
	c, s := stoppedJobSession(t)
	readyToFinish := make(chan struct{})
	var release sync.Once
	releaseJob := func() { release.Do(func() { close(readyToFinish) }) }
	t.Cleanup(releaseJob)
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "launch" {
			<-readyToFinish
		}
	}
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "stop" {
			releaseJob()
			<-c.Exec.Jobs()[0].Done()
		}
		if e.Kind == EventToolStart && e.ToolCallID == "retry" {
			h, err := session.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range h {
				if (m.Kind == "job" || m.ToolCallID == "stop") && strings.Contains(m.Content, "stopped-payload") {
					return
				}
			}
			t.Error("completed target had no persisted result when next tool started")
		}
	}
	_, err := runStoppedJobSteps(t, c,
		func() string {
			return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' stopped payload; : > ready; exec sleep 30`))
		},
		func() string {
			waitStoppedJobFile(t, c, "ready")
			return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("retry", "bash", `{"command":":"}`)}, nil, true)
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoppedJobLargeBatchFitsDefaultContext(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.ContextLimit = DefaultContextLimit
	c.Exec.OutputDir = t.TempDir()
	c.OnEvent = func(e Event) {
		if e.Kind != EventToolStart || e.ToolCallID != "stop" {
			return
		}
		for i := range 36 {
			ctx := context.Background()
			cmd := `for n in {1..12}; do printf '%01000d\n' 0; done; exit 2`
			if i >= 34 {
				ctx = tools.WithFailureKey(ctx, "repeatable-check")
			}
			if i == 35 {
				cmd = ":"
			}
			j, err := c.Exec.BashBackground(ctx, tools.BashArgs{Command: cmd, Background: true})
			if err != nil {
				t.Fatal(err)
			}
			<-j.Done()
		}
	}
	requests, err := runStoppedJobSteps(t, c,
		func() string { return toolCallSSE("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`)) },
		func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil || len(requests) < 3 {
		t.Fatalf("loud stop batch blocked the next model call: requests=%d delivered=%v err=%v", len(requests), c.Exec.Jobs()[0].Delivered(), err)
	}
}

func TestStoppedJobSessionlessBinaryCanCompact(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Session = nil
	c.ContextLimit = 15000
	requests, err := runStoppedJobSteps(t, c,
		func() string {
			return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '\377binary\n'; printf '%032000d' 0; : > ready; exec sleep 30`))
		},
		func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil || len(requests) < 3 {
		t.Fatalf("sessionless stopped binary result broke compaction: requests=%d err=%v", len(requests), err)
	}
}

func TestStoppedJobProviderShowsFailureBeforeRetry(t *testing.T) {
	c, _ := stoppedJobSession(t)
	args := `{"command":"if test -f ready; then printf '%s-%s' retry repaired; exit 0; fi; printf '%s-%s' initial output; : > ready; exec sleep 30"}`
	c.Exec.BackgroundThreshold = 150 * time.Millisecond
	requests, err := runStoppedJobSteps(t, c,
		func() string { return toolCallSSE("launch", "bash", args) },
		func() string {
			waitStoppedJobFile(t, c, "ready")
			return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("retry", "bash", args)}, nil, true)
		})
	if err != nil {
		t.Fatal(err)
	}
	stop, retry := strings.Index(requests[2], "initial-output"), strings.Index(requests[2], "retry-repaired")
	if stop < 0 || retry < 0 || stop > retry {
		t.Fatalf("provider sees stopped failure after successful retry: failure offset %d, retry offset %d", stop, retry)
	}
}
