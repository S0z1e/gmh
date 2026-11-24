package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
)

func TestSummaryEvictsOptionalFactsToFitButKeepsUnresolvedFailures(t *testing.T) {
	out, err := checkpoint.Observe("UNRESOLVED-FAILURE", 100)
	if err != nil {
		t.Fatal(err)
	}
	state := checkpoint.State{Version: checkpoint.Version, Failures: []checkpoint.Failure{{
		Fingerprint: strings.Repeat("a", 64), Source: "tool:test", Tool: "bash", Output: out,
	}}}
	for i := range 150 {
		state.Artifacts = append(state.Artifacts, checkpoint.Artifact{
			Kind: "workspace_path", Path: fmt.Sprintf("artifact-%03d-%s", i, strings.Repeat("x", 920)), Source: "tool:test",
		})
	}
	facts, err := checkpointMessage(state)
	if err != nil || len(facts.Content) <= 128<<10 {
		t.Fatalf("test checkpoint must exceed the summary ceiling: bytes=%d, err=%v", len(facts.Content), err)
	}
	prompt, err := BuildCompactPrompt([]llm.Message{facts}, []string{"Keep the source facts."})
	if err != nil || len(prompt) > 128<<10 || strings.Contains(prompt, "artifact-000") || !strings.Contains(prompt, "artifact-149") || !strings.Contains(prompt, "UNRESOLVED-FAILURE") {
		t.Fatalf("summary evicted the wrong content or exceeded its limit: bytes=%d, err=%v", len(prompt), err)
	}
	if _, err := checkpoint.EncodeForSummary(state, 64); err == nil {
		t.Fatal("discarded unresolved failure to fit a smaller limit")
	}
}

func TestContextOverflowDiscardsVisibleDraftBeforeCompaction(t *testing.T) {
	c := budgetConfig(t, nil)
	calls := 0
	c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			draft := budgetSSE("discard this draft", nil, nil, false)
			return budgetResponse(draft + `data: {"error":{"code":"context_length_exceeded","message":"context window exceeded"}}` + "\n\n"), nil
		}
		return budgetResponse(budgetSSE("replayed answer", nil, newInt(5), true)), nil
	})
	c.CompactHistory = func(context.Context, []llm.Message) (string, llm.Usage, error) {
		return "summary", llm.Usage{PromptTokens: 1, TotalTokens: 1}, nil
	}
	var events []Event
	c.OnEvent = func(e Event) { events = append(events, e) }
	answer, _, err := c.Run(t.Context(), []llm.Message{{Role: "user", Kind: "task", Content: "work"}})
	if err != nil || calls != 2 || answer != "replayed answer" {
		t.Fatalf("context overflow was not recovered: answer=%q, calls=%d, err=%v", answer, calls, err)
	}
	if len(events) < 3 || events[0].Kind != EventTextDelta || events[1].Kind != EventStepDiscard || events[2].Kind != EventStepUsage || events[0].StepID != events[1].StepID {
		t.Fatalf("draft not discarded before the first usage event: %+v", events)
	}
}

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

func TestSummaryCapsToolArgumentsWithoutSplittingUTF8(t *testing.T) {
	args := strings.Repeat("中", 10000) + "END-SENTINEL"
	prompt := testCompactPrompt(t, []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "a", Function: llm.FunctionCall{Name: "write", Arguments: args}}}}}, nil)
	if !strings.Contains(prompt, "END-SENTINEL") || len(prompt) > 2500 || !utf8.ValidString(prompt) {
		t.Fatal("summary tool arguments were not safely bounded")
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
	prompt := testCompactPrompt(t, history, nil)
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
	prompt := testCompactPrompt(t, history, nil)
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
	prompt := testCompactPrompt(t, history, extras)
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
	if prompt := testCompactPrompt(t, history, nil); strings.Contains(prompt, "Additional instructions") {
		t.Fatalf("extras block present with no extras:\n%s", prompt)
	}
	if prompt := testCompactPrompt(t, history, []string{}); strings.Contains(prompt, "Additional instructions") {
		t.Fatalf("extras block present with empty extras:\n%s", prompt)
	}
}

func testCompactPrompt(t *testing.T, history []llm.Message, extras []string) string {
	t.Helper()
	prompt, err := BuildCompactPrompt(history, extras)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func TestSummaryRequestHasIndependentBound(t *testing.T) {
	history := []llm.Message{{Role: "user", Kind: "checkpoint", Content: `{"version":1}`}}
	for i := 0; i < 10000; i++ {
		history = append(history, llm.Message{Role: "assistant", Content: strings.Repeat("x", 1000)})
	}
	history = append(history, llm.Message{Role: "tool", Content: "HEAD ERROR" + strings.Repeat("中", 10000) + "TAIL ERROR"})
	prompt, err := buildCompactPrompt(history, nil, 12000)
	if err != nil || len(prompt) > 12000 || !utf8.ValidString(prompt) || !strings.Contains(prompt, "TAIL ERROR") || !strings.Contains(prompt, "omitted") {
		t.Fatalf("unbounded or incomplete narration: bytes=%d err=%v", len(prompt), err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if n := estimatePromptTokens(request.Messages, nil); n > 2800 {
			t.Errorf("summary sent over budget: %d", n)
		}
		fmt.Fprint(w, textSSE("summary"))
	}))
	defer srv.Close()
	cfg := &Config{ContextLimit: 4000, Client: &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "test", MaxTokens: 500}}
	if _, _, err := cfg.Summarize(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.Summarize(context.Background(), history, []string{strings.Repeat("x", 12000)}); err == nil || calls.Load() != 1 {
		t.Fatalf("oversized extras called provider: %v calls=%d", err, calls.Load())
	}
	mandatory := []llm.Message{{Role: "user", Kind: "checkpoint", Content: strings.Repeat("x", 12000)}}
	if _, _, err := cfg.Summarize(context.Background(), mandatory, nil); err == nil || calls.Load() != 1 {
		t.Fatalf("oversized mandatory state called provider: %v calls=%d", err, calls.Load())
	}
}

func TestSummaryCannotDropSourcesWithoutStructuredFacts(t *testing.T) {
	history := []llm.Message{{Role: "user", Kind: "task", Content: strings.Repeat("preserve every instruction ", 1000)}}
	before := history[0].Content
	cfg := &Config{ContextLimit: 1000, Client: &llm.Client{MaxTokens: 100}}
	if _, _, err := cfg.Summarize(context.Background(), history, nil); err == nil || !strings.Contains(err.Error(), "input budget") {
		t.Fatalf("unprotected source text silently clipped: %v", err)
	}
	if history[0].Content != before {
		t.Fatal("failed summary changed source history")
	}
}

func TestSummaryReservesAnthropicDefaultOutput(t *testing.T) {
	cfg := &Config{ContextLimit: 16000, Client: &llm.Client{API: "anthropic"}}
	if _, _, err := cfg.Summarize(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "no context input budget") {
		t.Fatalf("default output budget was not reserved before dispatch: %v", err)
	}
}
