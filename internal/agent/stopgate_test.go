package agent

import (
	"context"
	"fmt"
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

// TestStopHookBlocksOnceThenEnds: the stop hook vetoes the first
// turn end ("go test failed"); the model answers again and the turn
// ends. The block reason is visible as a stop_blocked event, and the
// hook sees the final text it vetoed.
func TestStopHookBlocksOnceThenEnds(t *testing.T) {
	root := t.TempDir()
	hooks := &fakePluginRunner{stopBlocks: 1, stopReason: "go test failed"}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			fmt.Fprint(w, textSSE("all done"))
			return
		}
		fmt.Fprint(w, textSSE("fixed it"))
	}))
	defer srv.Close()
	cfg := &Config{
		Client: &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat", BaseURL: srv.URL},
		Exec:   &tools.Executor{Root: root, Plugins: hooks},
		Tools:  ToolDefs(),
		System: "test system",
	}
	var events []Event
	cfg.OnEvent = func(e Event) { events = append(events, e) }
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "fixed it" {
		t.Fatalf("final text = %q, want fixed it", text)
	}
	if calls != 2 {
		t.Fatalf("LLM calls = %d, want 2 (one veto, one retry)", calls)
	}
	if hooks.stopCalls != 2 {
		t.Fatalf("stop hook calls = %d, want 2", hooks.stopCalls)
	}
	blocked := 0
	for _, e := range events {
		if e.Kind == EventStopBlocked {
			blocked++
			if e.Text != "go test failed" {
				t.Fatalf("stop_blocked text = %q, want the hook reason", e.Text)
			}
		}
	}
	if blocked != 1 {
		t.Fatalf("stop_blocked events = %d, want 1", blocked)
	}
}

// TestStopHookCapAllowsEnd: a hook that always vetoes cannot trap
// the turn forever — after MaxStopBlocks vetoes the turn ends anyway.
func TestStopHookCapAllowsEnd(t *testing.T) {
	root := t.TempDir()
	hooks := &fakePluginRunner{stopBlocks: 99, stopReason: "always block"}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textSSE("done"))
	}))
	defer srv.Close()
	cfg := &Config{
		Client:   &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat", BaseURL: srv.URL},
		Exec:     &tools.Executor{Root: root, Plugins: hooks},
		Tools:    ToolDefs(),
		System:   "test system",
		MaxSteps: 20,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want done", text)
	}
	if calls != MaxStopBlocks+1 {
		t.Fatalf("LLM calls = %d, want %d (cap + final)", calls, MaxStopBlocks+1)
	}
}

// TestThreeFailedFixAttemptsMarkBlocked: the model calls read on a
// missing file three turns in a row; the third all-failed step ends
// the turn with a structured "blocked" reason instead of looping.
func TestThreeFailedFixAttemptsMarkBlocked(t *testing.T) {
	root := t.TempDir()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, toolCallSSE("1", "read", `{"path":"/nonexistent-xyz.txt"}`))
	}))
	defer srv.Close()
	cfg := &Config{
		Client:   &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat", BaseURL: srv.URL},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		MaxSteps: 10,
	}
	var events []Event
	cfg.OnEvent = func(e Event) { events = append(events, e) }
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("err = %v, want a blocked error", err)
	}
	if calls != MaxFailedFixAttempts {
		t.Fatalf("LLM calls = %d, want %d", calls, MaxFailedFixAttempts)
	}
	if cfg.LastTerminalReason != TerminalBlocked {
		t.Fatalf("LastTerminalReason = %q, want blocked", cfg.LastTerminalReason)
	}
	blocked := false
	for _, e := range events {
		if e.Kind == EventError && e.TerminalReason == TerminalBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("no error event with blocked reason; events: %+v", events)
	}
}

// TestFailedFixCounterResetsOnSuccess: a failed step followed by a
// successful read does not accumulate toward the blocked rule.
func TestFailedFixCounterResetsOnSuccess(t *testing.T) {
	root := t.TempDir()
	// One real file the model can read successfully on the second step.
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			fmt.Fprint(w, toolCallSSE("1", "read", `{"path":"/nonexistent-xyz.txt"}`))
		case 2:
			fmt.Fprint(w, toolCallSSE("2", "read", `{"path":"ok.txt"}`))
		default:
			fmt.Fprint(w, textSSE("done"))
		}
	}))
	defer srv.Close()
	cfg := &Config{
		Client:   &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat", BaseURL: srv.URL},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		MaxSteps: 10,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want done", text)
	}
	// The turn survived past the second (successful) step: no blocked error.
	if calls != 3 {
		t.Fatalf("LLM calls = %d, want 3 (fail, success, done)", calls)
	}
}
