package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

// Step-cap grace period: with MaxSteps 2 the model calls the same
// read twice, hits the cap, and gets one tool-less final call. The
// turn still returns the cap error with TerminalMaxTurns, but its
// final text is the grace answer, not the last tool step's text.
func TestRunStepCapGracePeriod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// The grace call: no tools were offered, so the model must
		// answer in text.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"best effort summary\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 2,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	text, total, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "read a.txt"}})
	if err == nil || !strings.Contains(err.Error(), "step cap reached") {
		t.Fatalf("Run error = %v, want the step-cap error", err)
	}
	if cfg.LastTerminalReason != TerminalMaxTurns {
		t.Fatalf("terminal reason = %q, want max_turns", cfg.LastTerminalReason)
	}
	if text != "best effort summary" {
		t.Fatalf("final text = %q, want the grace answer", text)
	}
	// The terminal error event must carry the grace text too: the
	// RPC server only sees the event stream, and without it the
	// GUI's agent_settled.text comes out empty on every capped turn.
	var errEvent *Event
	for i := range events {
		if events[i].Kind == EventError {
			errEvent = &events[i]
		}
	}
	if errEvent == nil {
		t.Fatal("no EventError emitted for the capped turn")
	}
	if errEvent.Text != "best effort summary" {
		t.Fatalf("error event text = %q, want the grace answer", errEvent.Text)
	}
	if errEvent.TerminalReason != TerminalMaxTurns {
		t.Fatalf("error event reason = %q, want max_turns", errEvent.TerminalReason)
	}
	if total.TotalTokens != 15+15+23 {
		t.Fatalf("total tokens = %d, want 53 (two steps + grace)", total.TotalTokens)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("LLM calls = %d, want 3 (two steps + one grace call)", got)
	}
}

// Blocked-grace summary: a turn that would settle `blocked` with an
// empty text now gets one tool-less final call, and its summary
// becomes the settled text. The model reads a missing file three
// times (three consecutive all-failed steps), then the grace call
// answers in text. The grace request must carry the blocked reason
// and the last error, so the summary names what actually failed.
func TestRunBlockedGracePeriod(t *testing.T) {
	root := t.TempDir()
	var calls int32
	var graceBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n == 4 {
			body, _ := io.ReadAll(r.Body)
			graceBody = string(body)
		}
		if n <= 3 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"missing.txt\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// The grace call: no tools were offered, so the model must
		// answer in text.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"blocked summary: the file was never found\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:  &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:    &tools.Executor{Root: root},
		Tools:   ToolDefs(),
		System:  "test system",
		Mode:    "auto",
		OnEvent: func(e Event) { events = append(events, e) },
	}
	text, total, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "read missing.txt"}})
	if err == nil || !strings.Contains(err.Error(), "task blocked") {
		t.Fatalf("Run error = %v, want the blocked error", err)
	}
	if cfg.LastTerminalReason != TerminalBlocked {
		t.Fatalf("terminal reason = %q, want blocked", cfg.LastTerminalReason)
	}
	if text != "blocked summary: the file was never found" {
		t.Fatalf("final text = %q, want the grace answer", text)
	}
	var errEvent *Event
	for i := range events {
		if events[i].Kind == EventError {
			errEvent = &events[i]
		}
	}
	if errEvent == nil {
		t.Fatal("no EventError emitted for the blocked turn")
	}
	if errEvent.Text != "blocked summary: the file was never found" {
		t.Fatalf("error event text = %q, want the grace answer", errEvent.Text)
	}
	if errEvent.TerminalReason != TerminalBlocked {
		t.Fatalf("error event reason = %q, want blocked", errEvent.TerminalReason)
	}
	if total.TotalTokens != 15+15+15+23 {
		t.Fatalf("total tokens = %d, want 68 (three steps + grace)", total.TotalTokens)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("LLM calls = %d, want 4 (three steps + one grace call)", got)
	}
	// The grace call's prompt names the block and the failed file.
	if !strings.Contains(graceBody, "blocked") || !strings.Contains(graceBody, "missing.txt") {
		t.Fatalf("grace request body lacks the blocked reason or the last error:\n%s", graceBody)
	}
}
