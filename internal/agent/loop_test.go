package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

// The loop must emit its event stream in order for a tool-call turn:
// text delta, per-step usage, tool start/end, final text, done.
func TestRunEmitsEventsInOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Reading \"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"a.txt\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
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
		MaxSteps: 5,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	text, total, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "read a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want %q", text, "done")
	}
	if total.TotalTokens != 38 {
		t.Fatalf("total tokens = %d, want 38", total.TotalTokens)
	}
	wantKinds := []EventKind{
		EventTextDelta, EventStepUsage, EventToolStart, EventToolEnd,
		EventTextDelta, EventStepUsage, EventDone,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantKinds), events)
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Fatalf("event %d kind = %s, want %s (all: %+v)", i, events[i].Kind, want, events)
		}
	}
	if events[0].Text != "Reading " {
		t.Fatalf("event 0 text = %q, want %q", events[0].Text, "Reading ")
	}
	if events[2].ToolName != "read" || events[2].ToolArgs != `{"path":"a.txt"}` {
		t.Fatalf("tool start = %s %s", events[2].ToolName, events[2].ToolArgs)
	}
	if events[3].ToolOutput != "1\thello\n2\t\n" {
		t.Fatalf("tool end output = %q, want numbered read output", events[3].ToolOutput)
	}
	if events[6].Text != "done" || events[6].Usage.TotalTokens != 38 {
		t.Fatalf("done event = %+v", events[6])
	}
}

// A denied tool call still emits tool start/end so renderers stay in sync.
func TestRunEmitsEventsForDeniedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"x.txt\\\",\\\"content\\\":\\\"hi\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// The mock always returns the same write tool call, so after the
	// denial the loop calls the API again, denies again, and hits the
	// step cap. That error is expected; the point is the events.
	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "suggest",
		MaxSteps: 2,
		Ask:      func(prompt string) (bool, error) { return false, nil },
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "write x.txt"}})
	// The second API call returns the same write tool call, which is denied
	// again; the step cap then ends the run. That error is expected here.
	if err == nil {
		t.Fatal("expected step-cap error after repeated denials")
	}
	var sawStart, sawEnd, sawApproval bool
	for _, e := range events {
		if e.Kind == EventToolStart && e.ToolName == "write" {
			sawStart = true
		}
		if e.Kind == EventToolEnd && e.ToolName == "write" {
			sawEnd = true
		}
		if e.Kind == EventApproval && e.Text != "" {
			sawApproval = true
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("denied tool call missing start/end events: %+v", events)
	}
	if !sawApproval {
		t.Fatalf("denied tool call missing approval event: %+v", events)
	}
}
