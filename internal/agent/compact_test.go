package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"minimalharness/internal/llm"
)

func TestShouldCompact(t *testing.T) {
	cases := []struct {
		lastPrompt, limit int
		want              bool
	}{
		{7000, 10000, true},      // exactly 70%
		{6999, 10000, false},     // just below 70%
		{100, 0, false},          // 0 limit falls back to default 128,000
		{800000, 1048576, true},  // explicitly configured 1M window
		{734002, 1048576, false}, // just below 70% of the configured window
		{734003, 1048576, true},  // 70% of the configured window (int truncation)
	}
	for _, c := range cases {
		if got := ShouldCompact(c.lastPrompt, c.limit); got != c.want {
			t.Errorf("ShouldCompact(%d, %d) = %v, want %v", c.lastPrompt, c.limit, got, c.want)
		}
	}
}

func TestSummarizeRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		calls  int32
	}{
		{"transient failure exhausts the configured budget", http.StatusForbidden, 2},
		{"invalid credentials are not retried", http.StatusUnauthorized, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := retryTestServer(t, 100, tc.status, "1")
			var events []Event
			cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "mock"}, MaxLLMAttempts: 2, OnEvent: func(e Event) { events = append(events, e) }}
			_, _, err := cfg.Summarize(context.Background(), []llm.Message{{Role: "user", Content: "task"}}, nil)
			if err == nil || atomic.LoadInt32(calls) != tc.calls || countKind(events, EventStepRetry) != int(tc.calls-1) {
				t.Fatalf("error = %v, calls = %d, events = %+v", err, atomic.LoadInt32(calls), events)
			}
			if tc.calls > 1 && (!strings.Contains(err.Error(), "after 2 attempts") || !strings.Contains(events[0].Text, "Compaction failed")) {
				t.Fatalf("retry cause/budget missing: %v; %+v", err, events)
			}
		})
	}
}

func TestSummarizeRetriesPrivateDraftWithoutDiscardingVisibleOutput(t *testing.T) {
	srv, calls := cutoffTestServer(t, 1, true)
	var events []Event
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "mock"}, OnEvent: func(e Event) { events = append(events, e) }}
	summary, _, err := cfg.Summarize(context.Background(), []llm.Message{{Role: "user", Content: "task"}}, nil)
	if err != nil || summary != "final answer" || atomic.LoadInt32(calls) != 2 {
		t.Fatalf("summary = %q, error = %v, calls = %d", summary, err, atomic.LoadInt32(calls))
	}
	if len(events) != 1 || events[0].Kind != EventStepRetry {
		t.Fatalf("private summary drafts leaked UI events: %+v", events)
	}
}

func TestSummarizeCancelledBackoffDoesNotRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, calls := retryTestServer(t, 100, http.StatusForbidden, "1")
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "mock"}, OnEvent: func(e Event) {
		if e.Kind == EventStepRetry {
			cancel()
		}
	}}
	_, _, err := cfg.Summarize(ctx, []llm.Message{{Role: "user", Content: "task"}}, nil)
	if err == nil || atomic.LoadInt32(calls) != 1 {
		t.Fatalf("cancelled summary retried: error = %v, calls = %d", err, atomic.LoadInt32(calls))
	}
}

func TestSummarizeRejectsTruncatedSummaryWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "mock"}}
	_, _, err := cfg.Summarize(context.Background(), []llm.Message{{Role: "user", Content: "task"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "truncated") || calls.Load() != 1 {
		t.Fatalf("truncated summary = %v, calls = %d", err, calls.Load())
	}
}

func TestBuildCompactPromptHasSixSections(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "add a test"},
		{Role: "assistant", Content: "done", ToolCalls: []llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "write", Arguments: "{}"}}}},
		{Role: "tool", Content: "created test.go", ToolCallID: "1"},
	}
	prompt := BuildCompactPrompt(history, nil)
	for _, section := range []string{
		"1. Task summary", "2. Key files", "3. Decisions",
		"4. Pending", "5. Errors and fixes", "6. Next steps",
	} {
		if !strings.Contains(prompt, section) {
			t.Fatalf("missing section %q", section)
		}
	}
	for _, want := range []string{"add a test", "done", "write", "created test.go"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("history content %q missing from prompt", want)
		}
	}
}

func TestBuildCompactPromptTruncatesToolResults(t *testing.T) {
	history := []llm.Message{
		{Role: "tool", Content: strings.Repeat("x", 2000), ToolCallID: "1"},
	}
	prompt := BuildCompactPrompt(history, nil)
	if strings.Contains(prompt, strings.Repeat("x", 2000)) {
		t.Fatal("tool result was not truncated in the summary prompt")
	}
	if !strings.Contains(prompt, "[truncated in summary prompt]") {
		t.Fatal("truncation marker missing")
	}
}

// Phase 4: session_before_compact extras go after section 6 and
// before the conversation, so a plugin steers what the summary
// keeps without replacing it.
func TestBuildCompactPromptExtrasPosition(t *testing.T) {
	history := []llm.Message{{Role: "user", Content: "do the thing"}}
	extras := []string{
		"[plugin plan] Keep the current plan in the summary.",
		"[plugin todo] Keep the open todos.",
	}
	prompt := BuildCompactPrompt(history, extras)
	sec6 := strings.Index(prompt, "6. Next steps")
	extra := strings.Index(prompt, "Additional instructions from plugins:")
	first := strings.Index(prompt, "[plugin plan]")
	second := strings.Index(prompt, "[plugin todo]")
	conv := strings.Index(prompt, "--- conversation ---")
	if sec6 < 0 || extra < 0 || first < 0 || second < 0 || conv < 0 {
		t.Fatalf("prompt missing a block:\n%s", prompt)
	}
	if !(sec6 < extra && extra < first && first < second && second < conv) {
		t.Fatalf("extras out of order (want section 6, extras block, plan, todo, conversation):\n%s", prompt)
	}
}

func TestBuildCompactPromptNoExtrasBlock(t *testing.T) {
	history := []llm.Message{{Role: "user", Content: "do the thing"}}
	if prompt := BuildCompactPrompt(history, nil); strings.Contains(prompt, "Additional instructions") {
		t.Fatalf("extras block present with no extras:\n%s", prompt)
	}
	if prompt := BuildCompactPrompt(history, []string{}); strings.Contains(prompt, "Additional instructions") {
		t.Fatalf("extras block present with empty extras:\n%s", prompt)
	}
}
