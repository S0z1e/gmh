package agent

import (
	"context"
	"encoding/json"
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

func TestFailureMessages(t *testing.T) {
	if got := FailureNote("bash", 2); !strings.Contains(got, "already failed 2 times") || !strings.Contains(got, "One more failure") {
		t.Fatalf("2nd-failure note = %q", got)
	}
	if got := FailureNote("bash", 3); !strings.Contains(got, "already failed 3 times") || !strings.Contains(got, "will be refused") {
		t.Fatalf("3rd-failure note = %q", got)
	}
	if got := RepeatedFailureDeniedMessage("bash", 3); !strings.Contains(got, RepeatedToolFailureReason) || !strings.HasPrefix(got, "Error:") {
		t.Fatalf("denial message = %q, want an Error with reason %q", got, RepeatedToolFailureReason)
	}
	if RepeatedToolFailureReason == string(TerminalRepeatedToolCallDenied) {
		t.Fatalf("breaker reason %q must differ from the doom-loop reason", RepeatedToolFailureReason)
	}
}

// jsonString renders s as a JSON string literal, for embedding
// an arguments object inside the fake server's SSE payload.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Successful reads no longer hide repeated failures. The third failed
// execution settles blocked and receives one tool-free grace report.
func TestRunRepeatFailureNoProgressStopsWithGrace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failArgs, _ := json.Marshal(map[string]string{"command": "echo tick >> tick.txt; exit 1"})
	readArgs, _ := json.Marshal(map[string]string{"path": "a.txt"})
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		var body string
		switch n {
		case 1, 2, 4:
			body = fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":%s}}]}}]}\n\n", jsonString(string(failArgs)))
		case 3:
			body = fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":%s}}]}}]}\n\n", jsonString(string(readArgs)))
		default:
			body = "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n"
		}
		fmt.Fprint(w, body)
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var toolEnds []Event
	cfg := &Config{
		Client: &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:   &tools.Executor{Root: root},
		Tools:  ToolDefs(),
		System: "test system",
		Mode:   "auto",
		OnEvent: func(e Event) {
			if e.Kind == EventToolEnd {
				toolEnds = append(toolEnds, e)
			}
		},
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "run the failing command four times"}})
	if err == nil {
		t.Fatal("no-progress failures did not block")
	}
	if cfg.LastTerminalReason != TerminalBlocked {
		t.Fatalf("terminal reason = %q, want blocked", cfg.LastTerminalReason)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want \"done\"", text)
	}
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("LLM calls = %d, want 5 (four steps + grace)", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "tick.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Fatalf("tick.txt has %d lines, want 3 — no extra failing attempt may run", lines)
	}
	// The second failure still provides an early warning.
	var warned bool
	for _, e := range toolEnds {
		if strings.Contains(e.ToolOutput, "already failed 2 times") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no tool_end warned about the 2nd failure: %+v", toolEnds)
	}
}

// Counts reset every turn: a fingerprint that failed 3 times in
// one turn runs again in the next turn instead of being denied.
func TestFailureBreakerResetsEachTurn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failArgs, _ := json.Marshal(map[string]string{"command": "echo tick >> tick.txt; exit 1"})
	readArgs, _ := json.Marshal(map[string]string{"path": "a.txt"})
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		var body string
		switch n {
		case 1, 3, 5, 7:
			body = fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":%s}}]}}]}\n\n", jsonString(string(failArgs)))
		case 2, 4:
			body = fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":%s}}]}}]}\n\n", jsonString(string(readArgs)))
		default:
			body = "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n"
		}
		fmt.Fprint(w, body)
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client: &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:   &tools.Executor{Root: root},
		Tools:  ToolDefs(),
		System: "test system",
		Mode:   "auto",
	}
	// Turn 1: the bash call fails 3 times (reads in between).
	if _, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "turn one"}}); err == nil || cfg.LastTerminalReason != TerminalBlocked {
		t.Fatalf("turn 1 error = %v", err)
	}
	// Turn 2: the same call must EXECUTE again — a leaked count
	// would deny it, and tick.txt would stay at 3 lines.
	if _, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "turn two"}}); err != nil {
		t.Fatalf("turn 2 error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "tick.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 4 {
		t.Fatalf("tick.txt has %d lines, want 4 — the turn-2 call must have run", lines)
	}
}
