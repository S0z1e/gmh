package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

// retryTestServer returns a server that fails the first failCount
// requests with status, then answers a valid SSE success. retryAfter
// is sent as a Retry-After header on the failures ("" sends none).
func retryTestServer(t *testing.T, failCount int32, status int, retryAfter string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= failCount {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, "RBAC: access denied")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func retryTestConfig(t *testing.T, srv *httptest.Server, events *[]Event) *Config {
	t.Helper()
	return &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 1,
		OnEvent:  func(e Event) { *events = append(*events, e) },
	}
}

func countKind(events []Event, kind EventKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func retryAttempts(events []Event) []int {
	var out []int
	for _, e := range events {
		if e.Kind == EventStepRetry {
			out = append(out, e.RetryAttempt)
		}
	}
	return out
}

// A 403 twice, then success: the turn ends normally after exactly
// 3 LLM calls, with two step_retry events (attempts 2 and 3).
func TestRunRetriesTransient403(t *testing.T) {
	srv, calls := retryTestServer(t, 2, 403, "")
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err != nil {
		t.Fatalf("Run error = %v, want nil after retries", err)
	}
	if text != "hello" {
		t.Fatalf("final text = %q, want \"hello\"", text)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("LLM calls = %d, want 3 (2 failures + success)", got)
	}
	if got := retryAttempts(events); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("step_retry attempts = %v, want [2 3]", got)
	}
	if cfg.LastTerminalReason != TerminalEndTurn {
		t.Fatalf("terminal reason = %q, want end_turn", cfg.LastTerminalReason)
	}
}

// A 403 every time: the turn fails after exactly 4 attempts, and
// the error says so.
func TestRunRetriesExhausted(t *testing.T) {
	srv, calls := retryTestServer(t, 100, 403, "")
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err == nil || !strings.Contains(err.Error(), "after 4 attempts") {
		t.Fatalf("Run error = %v, want it to contain \"after 4 attempts\"", err)
	}
	if got := atomic.LoadInt32(calls); got != 4 {
		t.Fatalf("LLM calls = %d, want 4", got)
	}
	if got := countKind(events, EventStepRetry); got != 3 {
		t.Fatalf("step_retry events = %d, want 3", got)
	}
	if cfg.LastTerminalReason != TerminalError {
		t.Fatalf("terminal reason = %q, want error", cfg.LastTerminalReason)
	}
}

// A 401 is never retried: exactly one call, no step_retry event.
func TestRunNeverRetries401(t *testing.T) {
	srv, calls := retryTestServer(t, 100, 401, "")
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err == nil || strings.Contains(err.Error(), "after") {
		t.Fatalf("Run error = %v, want a plain one-attempt error", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("LLM calls = %d, want 1", got)
	}
	if got := countKind(events, EventStepRetry); got != 0 {
		t.Fatalf("step_retry events = %d, want 0", got)
	}
}

// A stream that emits one text delta and then closes without [DONE]
// is never retried: the text was already exposed.
func TestRunNeverRetriesAfterOutput(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Close without [DONE].
	}))
	defer srv.Close()
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err == nil || !strings.Contains(err.Error(), "stream ended without [DONE]") {
		t.Fatalf("Run error = %v, want the cut-off stream error", err)
	}
	if text != "" {
		t.Fatalf("final text = %q, want empty: the partial text never became an answer", text)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("LLM calls = %d, want 1", got)
	}
	if got := countKind(events, EventStepRetry); got != 0 {
		t.Fatalf("step_retry events = %d, want 0", got)
	}
}

// A 429 with Retry-After: 1 twice, then success: 3 calls, and the
// waits follow Retry-After (~2s total), not the 2s+4s backoff.
func TestRunRetriesHonourRetryAfter(t *testing.T) {
	srv, calls := retryTestServer(t, 2, 429, "1")
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	start := time.Now()
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run error = %v, want nil after retries", err)
	}
	if text != "hello" {
		t.Fatalf("final text = %q, want \"hello\"", text)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("LLM calls = %d, want 3", got)
	}
	if elapsed < 1*time.Second || elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want ~2s (two 1s Retry-After waits, not 2s+4s backoff)", elapsed)
	}
	for _, e := range events {
		if e.Kind == EventStepRetry && !strings.Contains(e.Text, "retrying in 1s") {
			t.Fatalf("step_retry text = %q, want it to say \"retrying in 1s\"", e.Text)
		}
	}
}
