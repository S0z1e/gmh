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

	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 2,
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
	if total.TotalTokens != 15+15+23 {
		t.Fatalf("total tokens = %d, want 53 (two steps + grace)", total.TotalTokens)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("LLM calls = %d, want 3 (two steps + one grace call)", got)
	}
}
