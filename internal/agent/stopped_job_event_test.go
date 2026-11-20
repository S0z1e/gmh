package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobSuccessEventRequiresSavedOutcome(t *testing.T) {
	c, s := stoppedJobSession(t)
	successes := 0
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "stop" && e.ToolStatus == tools.ResultSuccess {
			successes++
			h, err := session.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range h {
				if m.Kind == "job" && strings.Contains(m.Content, "stopped-payload") {
					return
				}
			}
			t.Error("success reached event consumer with no durable stop outcome")
		}
	}
	_, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' stopped payload; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	if successes != 1 {
		t.Fatalf("success events=%d, want one", successes)
	}
}

func TestStoppedJobUnfinishedStopCannotReportSuccess(t *testing.T) {
	c, s := stoppedJobSession(t)
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "launch" {
			<-release
		}
	}
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolStart && e.ToolCallID == "stop" {
			j, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: ":", Background: true})
			if err != nil {
				t.Fatal(err)
			}
			<-j.Done()
		}
		if e.Kind == EventToolBackgroundDone && e.JobID == "bg-2" {
			finish()
			<-c.Exec.Jobs()[0].Done()
		}
		if e.Kind != EventToolStart || e.ToolCallID != "retry" {
			return
		}
		h, err := session.Load(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		var stop, result bool
		for _, m := range h {
			stop = stop || m.Role == "tool" && m.ToolCallID == "stop" && !m.ToolError
			result = result || m.Kind == "job" && strings.Contains(m.Content, "stopped-payload")
		}
		if stop && !result {
			t.Error("later tool started with successful persisted stop but no original outcome")
		}
	}
	_, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' stopped payload; : > ready; exec sleep 30`))
	}, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("retry", "bash", `{"command":":"}`)}, nil, true)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoppedJobLargeDisplayKeepsOriginalHeadAndTail(t *testing.T) {
	hist := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop)}}, {Role: "tool", ToolCallID: "stop", InvocationID: "s", Content: "Stopped"}}
	in := fmt.Sprintf("Background job bg-1 finished with error: exit: exit status 2 (exit 2).\nOutput:\n[bash output: 20 lines, 1000 bytes; full output in %s; cwd: /workspace]\nORIGINAL-HEAD\n%s\nORIGINAL-TAIL\n%s", "/workspace/"+strings.Repeat("long-dir/", 22)+"out.log", strings.Repeat("x", 1000), FailureNote("bash", 3))
	for range 100 {
		hist = append(hist, llm.Message{Role: "user", Kind: "job", Content: in, Observation: &llm.ToolObservation{BeforeInvocationID: "s"}})
	}
	tool := (&Config{}).promptView(hist)[1].Content
	if !strings.Contains(tool, "ORIGINAL-HEAD") || !strings.Contains(tool, "ORIGINAL-TAIL") {
		t.Fatalf("head=%v tail=%v, wire body length=%d", strings.Contains(tool, "ORIGINAL-HEAD"), strings.Contains(tool, "ORIGINAL-TAIL"), len(tool))
	}
}

func TestStoppedJobFinalizingTargetDefersRemainingTools(t *testing.T) {
	c, s := stoppedJobSession(t)
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "launch" {
			<-release
		}
	}
	deferred := false
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "stop" && e.ToolStatus != tools.ResultDeferred {
			t.Errorf("unfinished stop status=%q, want deferred", e.ToolStatus)
		}
		if e.Kind == EventToolEnd && e.ToolCallID == "later" {
			deferred = e.ToolErr != nil
			if _, err := os.Stat(filepath.Join(c.Exec.Root, "later")); !os.IsNotExist(err) {
				t.Errorf("later tool executed before the target outcome was available: %v", err)
			}
			finish()
			<-c.Exec.Jobs()[0].Done()
		}
	}
	requests, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' stopped payload; : > ready; exec sleep 30`))
	}, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop), budgetCall("later", "bash", `{"command":": > later"}`)}, nil, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !deferred {
		t.Fatal("remaining call was not deferred")
	}
	var next struct{ Messages []llm.Message }
	if err := json.Unmarshal([]byte(requests[2]), &next); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range next.Messages {
		found = found || m.Role == "tool" && m.ToolCallID == "later" && strings.Contains(m.Content, "Not executed")
	}
	if !found {
		t.Fatal("next provider request has no result for the deferred call")
	}
	h, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range h {
		if m.Role == "tool" && m.ToolCallID == "later" {
			if m.Kind != notExecutedKind || !m.ToolError {
				t.Fatalf("undispatched call has kind=%q error=%v", m.Kind, m.ToolError)
			}
			return
		}
	}
	t.Fatal("missing deferred tool record")
}

func TestStoppedJobBinaryDisplayFitsWireCap(t *testing.T) {
	c, s := stoppedJobSession(t)
	requests, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`i=0; while [ "$i" -lt 30 ]; do printf '%01000d\n' 0; i=$((i+1)); done | LC_ALL=C tr 0 '\377'; : > ready; exec sleep 30`))
	}, func() string { waitStoppedJobFile(t, c, "ready"); return toolCallSSE("stop", "bash", stoppedJobStop) })
	if err != nil {
		t.Fatal(err)
	}
	var req struct{ Messages []llm.Message }
	if err := json.Unmarshal([]byte(requests[2]), &req); err != nil {
		t.Fatal(err)
	}
	for _, m := range req.Messages {
		if m.Role == "tool" && m.ToolCallID == "stop" {
			if len(m.Content) > toolResultMaxChars || !utf8.ValidString(m.Content) {
				t.Fatalf("stop display %d bytes after JSON decoding, cap %d", len(m.Content), toolResultMaxChars)
			}
			h, err := session.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, saved := range c.promptView(h) {
				if saved.Role == "tool" && saved.ToolCallID == "stop" {
					if saved.Content != m.Content {
						t.Fatal("live and replay display differ after binary normalization")
					}
					return
				}
			}
			t.Fatal("no replayed stop tool reply")
			return
		}
	}
	t.Fatal("no stop tool reply")
}

func TestStoppedJobPruningUsesRenderedBudget(t *testing.T) {
	c := &Config{ContextLimit: 80000}
	hist := []llm.Message{{Role: "user", Kind: "task", Content: "work"}}
	hist = appendFixtureBatch(hist, 1, 32000)
	hist = appendFixtureBatch(hist, 2, 32000)
	hist = appendFixtureBatch(hist, 3, 32000)
	hist = append(hist, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop)}}, llm.Message{Role: "tool", ToolCallID: "stop", InvocationID: "s", Content: "Stopped"})
	output := strings.Repeat("x", 12000)
	excerpt, err := checkpoint.Observe(output, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 70 {
		hist = append(hist, llm.Message{Role: "user", Kind: "job", Content: output, ToolError: true, Observation: &llm.ToolObservation{Tool: "bash", Fingerprint: factDigest(fmt.Sprint(i)), Failure: &excerpt, BeforeInvocationID: "s"}})
	}
	if !c.needsCompact(hist) {
		t.Fatal("fixture must need compaction")
	}
	prunedManually := append([]llm.Message(nil), hist...)
	prunedManually[2].Content = prunedToolResultPlaceholder
	prunedManually[4].Content = prunedToolResultPlaceholder
	prunedManually[6].Content = prunedToolResultPlaceholder
	if n := c.contextTokens(c.promptView(prunedManually), nil); n > c.contextBudget()*3/5 {
		t.Fatalf("manual prune does not meet lower target: %d", n)
	}
	if _, ok := c.pruneCheckpoint(hist, nil); !ok {
		t.Error("raw job outputs prevented lightweight prune although rendered budget fit")
	}
	if _, _, err := c.compactInLoop(context.Background(), hist, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedJobStructuredPreviewPreservesActualOutput(t *testing.T) {
	h := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{budgetCall("stop", "bash", stoppedJobStop)}}, {Role: "tool", ToolCallID: "stop", InvocationID: "s", Content: "Stopped"}}
	for range 100 {
		h = append(h, llm.Message{Role: "user", Kind: "job", Content: "decorated legacy display", Observation: &llm.ToolObservation{BeforeInvocationID: "s", OutputFile: "/workspace/" + strings.Repeat("long-dir/", 100) + "out.log", JobDisplay: &llm.JobDisplay{
			Header: "Background job failed: " + strings.Repeat("error decoration", 40),
			Head:   "ACTUAL-HEAD\n" + strings.Repeat("中", 1000), Tail: strings.Repeat("末", 1000) + "\nACTUAL-TAIL", Note: strings.Repeat("warning", 1000),
		}}})
	}
	head, tail := 0, 0
	for _, m := range (&Config{}).promptView(h) {
		if !strings.Contains(m.Content, "[Background outcomes from stop call") {
			continue
		}
		if len(m.Content) > toolResultMaxChars || !utf8.ValidString(m.Content) {
			t.Fatalf("invalid or oversized display: %d bytes", len(m.Content))
		}
		head += strings.Count(m.Content, "ACTUAL-HEAD")
		tail += strings.Count(m.Content, "ACTUAL-TAIL")
	}
	if head != 100 || tail != 100 {
		t.Fatalf("actual output count: head=%d tail=%d, want 100 each", head, tail)
	}
}
