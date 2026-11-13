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
	if got := countKind(events, EventStepDiscard); got != 0 {
		t.Fatalf("step_discard events = %d, want 0: nothing was streamed before the 403s", got)
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
	if got := countKind(events, EventStepDiscard); got != 0 {
		t.Fatalf("step_discard events = %d, want 0: nothing was streamed before the 403s", got)
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

// cutoffTestServer returns a server that cuts off the first
// cutCount requests after one delta, then answers a valid SSE
// success. When thinking is true the cut-off delta is a
// reasoning_content delta ("partial-thinking") and the success
// streams thinking ("real-thinking") before its text; otherwise the
// cut-off delta is text ("partial-") and the success is text only.
func cutoffTestServer(t *testing.T, cutCount int32, thinking bool) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= cutCount {
			if thinking {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"partial-thinking\"}}]}\n\n")
			} else {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial-\"}}]}\n\n")
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Close without [DONE]: a mid-stream cut-off.
			return
		}
		if thinking {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"real-thinking\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"final answer\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func discardAttempts(events []Event) []int {
	var out []int
	for _, e := range events {
		if e.Kind == EventStepDiscard {
			out = append(out, e.RetryAttempt)
		}
	}
	return out
}

func eventOrder(events []Event, kinds ...EventKind) []EventKind {
	var out []EventKind
	for _, e := range events {
		for _, k := range kinds {
			if e.Kind == k {
				out = append(out, e.Kind)
			}
		}
	}
	return out
}

// A stream that emits one text delta and then cuts off IS retried:
// the partial reply is discarded first, then the replay succeeds.
// The final text is exactly the replay's text — "partial-" never
// becomes part of the answer.
func TestRunRetriesCutOffAfterOutput(t *testing.T) {
	srv, calls := cutoffTestServer(t, 1, false)
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err != nil {
		t.Fatalf("Run error = %v, want nil after the discard + retry", err)
	}
	if text != "final answer" {
		t.Fatalf("final text = %q, want \"final answer\"", text)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("LLM calls = %d, want 2 (1 cut-off + success)", got)
	}
	if got := discardAttempts(events); len(got) != 1 || got[0] != 2 {
		t.Fatalf("step_discard attempts = %v, want [2]", got)
	}
	if got := retryAttempts(events); len(got) != 1 || got[0] != 2 {
		t.Fatalf("step_retry attempts = %v, want [2]", got)
	}
	// The discard must precede the retry: a renderer drops the
	// partial text before the replay's deltas arrive.
	order := eventOrder(events, EventStepDiscard, EventStepRetry)
	if len(order) != 2 || order[0] != EventStepDiscard || order[1] != EventStepRetry {
		t.Fatalf("discard/retry order = %v, want [step_discard step_retry]", order)
	}
	if cfg.LastTerminalReason != TerminalEndTurn {
		t.Fatalf("terminal reason = %q, want end_turn", cfg.LastTerminalReason)
	}
}

// A stream that always cuts off after a delta: the turn fails after
// exactly 4 attempts, with a discard before each retry.
func TestRunCutOffAlwaysFails(t *testing.T) {
	srv, calls := cutoffTestServer(t, 100, false)
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err == nil || !strings.Contains(err.Error(), "after 4 attempts") {
		t.Fatalf("Run error = %v, want it to contain \"after 4 attempts\"", err)
	}
	if got := atomic.LoadInt32(calls); got != 4 {
		t.Fatalf("LLM calls = %d, want 4", got)
	}
	if got := countKind(events, EventStepDiscard); got != 3 {
		t.Fatalf("step_discard events = %d, want 3", got)
	}
	if got := countKind(events, EventStepRetry); got != 3 {
		t.Fatalf("step_retry events = %d, want 3", got)
	}
	if cfg.LastTerminalReason != TerminalError {
		t.Fatalf("terminal reason = %q, want error", cfg.LastTerminalReason)
	}
}

// A cut-off after a thinking delta: the discarded attempt's thinking
// must not leak into the replay's ThinkingEnd.
func TestRunCutOffResetsThinking(t *testing.T) {
	srv, calls := cutoffTestServer(t, 1, true)
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err != nil {
		t.Fatalf("Run error = %v, want nil after the discard + retry", err)
	}
	if text != "final answer" {
		t.Fatalf("final text = %q, want \"final answer\"", text)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("LLM calls = %d, want 2 (1 cut-off + success)", got)
	}
	var thinkingEnds []string
	for _, e := range events {
		if e.Kind == EventThinkingEnd {
			thinkingEnds = append(thinkingEnds, e.Text)
		}
	}
	if len(thinkingEnds) != 1 || thinkingEnds[0] != "real-thinking" {
		t.Fatalf("thinking_end texts = %q, want exactly [\"real-thinking\"]", thinkingEnds)
	}
	if got := countKind(events, EventStepDiscard); got != 1 {
		t.Fatalf("step_discard events = %d, want 1", got)
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

// idleTestServer returns a server that stalls the first stallCount
// requests — one text delta, then silence for stallFor — and then
// answers a valid SSE success.
func idleTestServer(t *testing.T, stallCount int32, stallFor time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= stallCount {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial-\"}}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(stallFor) // silence: the idle timeout must fire, not the sleep
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"final answer\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func idleRetryTestConfig(t *testing.T, srv *httptest.Server, events *[]Event, idle time.Duration) *Config {
	t.Helper()
	return &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat", IdleTimeout: idle, Timeout: 10 * time.Second},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 1,
		OnEvent:  func(e Event) { *events = append(*events, e) },
	}
}

// A stream that stalls after a text delta twice, then succeeds: the
// stalled attempts are discarded and retried exactly like a
// mid-stream cut-off, and the final text is the replay's text.
func TestRunRetriesIdleStallAfterOutput(t *testing.T) {
	srv, calls := idleTestServer(t, 2, 600*time.Millisecond)
	var events []Event
	cfg := idleRetryTestConfig(t, srv, &events, 100*time.Millisecond)
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err != nil {
		t.Fatalf("Run error = %v, want nil after the discard + retry", err)
	}
	if text != "final answer" {
		t.Fatalf("final text = %q, want \"final answer\"", text)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("LLM calls = %d, want 3 (2 stalls + success)", got)
	}
	if got := discardAttempts(events); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("step_discard attempts = %v, want [2 3]", got)
	}
	if got := retryAttempts(events); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("step_retry attempts = %v, want [2 3]", got)
	}
	if got := eventOrder(events, EventStepDiscard, EventStepRetry); len(got) != 4 || got[0] != EventStepDiscard || got[1] != EventStepRetry || got[2] != EventStepDiscard || got[3] != EventStepRetry {
		t.Fatalf("discard/retry order = %v, want alternating discard, retry, discard, retry", got)
	}
	if cfg.LastTerminalReason != TerminalEndTurn {
		t.Fatalf("terminal reason = %q, want end_turn", cfg.LastTerminalReason)
	}
}

// A stream that stalls on every attempt: the turn fails after
// exactly 4 attempts, naming the idle timeout.
func TestRunIdleStallAlwaysFails(t *testing.T) {
	srv, calls := idleTestServer(t, 100, 600*time.Millisecond)
	var events []Event
	cfg := idleRetryTestConfig(t, srv, &events, 100*time.Millisecond)
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "say hello"}})
	if err == nil || !strings.Contains(err.Error(), "after 4 attempts") {
		t.Fatalf("Run error = %v, want it to contain \"after 4 attempts\"", err)
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Fatalf("Run error = %v, want it to name the idle timeout", err)
	}
	if got := atomic.LoadInt32(calls); got != 4 {
		t.Fatalf("LLM calls = %d, want 4", got)
	}
	if got := countKind(events, EventStepDiscard); got != 3 {
		t.Fatalf("step_discard events = %d, want 3", got)
	}
	if got := countKind(events, EventStepRetry); got != 3 {
		t.Fatalf("step_retry events = %d, want 3", got)
	}
	if cfg.LastTerminalReason != TerminalError {
		t.Fatalf("terminal reason = %q, want error", cfg.LastTerminalReason)
	}
}
