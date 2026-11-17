package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTraceWireUsagePreservesInvalidAndOmittedTotals(t *testing.T) {
	for _, total := range []int{-1, 20} {
		o := &requestObservation{}
		ctx := context.WithValue(context.Background(), traceKey{}, o)
		traceWireUsage(ctx, Usage{TotalTokens: total}, fmt.Sprintf(`{"usage":{"total_tokens":%d}}`, total), false)
		traceWireUsage(ctx, Usage{PromptTokens: 10}, `{"usage":{"prompt_tokens":10}}`, false)
		if o.trace.Usage.TotalTokens != total || o.trace.TokensComplete || o.trace.PriceCountsComplete {
			t.Fatalf("omitted total lost or completed: %+v", o.trace)
		}
	}
}

func TestRequestTraceComparesActualWireRequests(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "upstream-123")
		w.Header().Set("Authorization", "secret-response-header")
		w.WriteHeader(status)
		if status != 200 {
			fmt.Fprint(w, "secret-error-body")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":100,\"total_tokens\":101,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	var traces []RequestTrace
	ctx := WithRequestObserver(context.Background(), func(trace RequestTrace) { traces = append(traces, trace) })
	c := &Client{BaseURL: srv.URL, Model: "test-model", APIKey: "secret-api-key", PromptCacheKey: "secret-session"}
	msgs := []Message{{Role: "system", Content: "secret-system"}, {Role: "user", Content: "secret-user"}}
	defs := []ToolDef{Tool("read", "read files", `{"type":"object"}`)}
	call := func() {
		t.Helper()
		_, _, err := c.Chat(ctx, msgs, defs, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	call()
	msgs = append(msgs, Message{Role: "assistant", Content: "secret-assistant"})
	call()
	call()
	msgs[1].Content = "changed-user"
	call()
	defs[0].Function.Description = "changed description"
	call()
	c.Effort = "high"
	call()
	c.PromptCacheKey = "another-session"
	call()
	want := []string{"baseline", "append", "identical", "rewrite", "identical", "identical", "baseline"}
	for i, trace := range traces {
		if trace.Relation != want[i] {
			t.Fatalf("trace %d relation=%s, want %s", i, trace.Relation, want[i])
		}
		if trace.Outcome != "complete" || trace.StatusCode != 200 || trace.UpstreamRequestID != "upstream-123" {
			t.Fatalf("trace %d: %+v", i, trace)
		}
		if trace.Usage == nil || !trace.Usage.CacheUsageReported {
			t.Fatalf("usage missing: %+v", trace)
		}
		if i > 0 && trace.PreviousID != traces[i-1].ID {
			t.Fatal("broken correlation")
		}
	}
	if traces[1].SharedMessages != 2 || traces[3].FirstChangedMessage == nil || *traces[3].FirstChangedMessage != 1 || !traces[4].ToolsChanged || !traces[5].SettingsChanged || !traces[6].ScopeChanged {
		t.Fatalf("wrong comparison: %+v", traces)
	}
	b, _ := json.Marshal(traces)
	for _, secret := range []string{"secret-", "changed-user", "changed description", srv.URL} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("trace leaked %s", secret)
		}
	}
	status = http.StatusForbidden
	_, _, err := c.Chat(ctx, msgs, defs, nil)
	if err == nil {
		t.Fatal("wanted HTTP error")
	}
	last := traces[len(traces)-1]
	if last.Outcome != "error" || last.StatusCode != 403 || last.Usage != nil {
		t.Fatalf("failed trace: %+v", last)
	}
	b, _ = json.Marshal(last)
	if strings.Contains(string(b), "secret-") {
		t.Fatal("error leaked to trace")
	}
}

func TestRequestTraceRetainsUsageOnInterruptedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":1,\"cached_tokens\":80}}\n\n")
	}))
	defer srv.Close()
	var trace RequestTrace
	c := &Client{BaseURL: srv.URL, APIKey: "test", Model: "model"}
	ctx := WithRequestObserver(context.Background(), func(v RequestTrace) { trace = v })
	_, _, err := c.Chat(ctx, []Message{{Role: "user", Content: "test"}}, nil, nil)
	if err == nil || trace.Outcome != "error" || trace.Usage == nil || trace.Usage.CachedTokens != 80 || trace.Usage.TotalTokens != 101 {
		t.Fatalf("err=%v trace=%+v", err, trace)
	}
}

func TestRequestTraceDisabled(t *testing.T) {
	c := &Client{}
	c.traceRequest(context.Background(), []byte(`{"messages":[]}`))
	if c.previousTrace != nil {
		t.Fatal("tracking enabled without observer")
	}
}

func TestRequestTraceUsageEventsAcrossAPIs(t *testing.T) {
	for _, tc := range []struct {
		name, api, events   string
		wantError, hasUsage bool
		prompt, cached      int
		reported            bool
	}{
		{"chat_no_usage", "chat", "data: [DONE]\n\n", false, false, 0, 0, false},
		{"chat_null_usage", "chat", "data: {\"usage\":null}\n\ndata: [DONE]\n\n", false, false, 0, 0, false},
		{"anthropic_no_usage", "anthropic", "data: {\"type\":\"message_stop\"}\n\n", false, false, 0, 0, false},
		{"anthropic_cutoff", "anthropic", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":90,\"cache_creation_input_tokens\":20}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n", true, true, 120, 90, true},
		{"responses_incomplete", "responses", "data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":3,\"total_tokens\":103,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n", false, true, 100, 0, true},
		{"responses_cutoff", "responses", "data: {\"type\":\"response.created\",\"response\":{\"usage\":{\"input_tokens\":100,\"input_tokens_details\":{\"cached_tokens\":80}}}}\n\n", true, true, 100, 80, true},
		{"responses_no_usage", "responses", "data: {\"type\":\"response.completed\",\"response\":{}}\n\n", false, false, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.events) }))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, APIKey: "test", Model: "model", API: tc.api}
			var trace RequestTrace
			ctx := WithRequestObserver(context.Background(), func(v RequestTrace) { trace = v })
			_, usage, err := c.Chat(ctx, []Message{{Role: "user", Content: "test"}}, nil, nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v", err)
			}
			if (trace.Usage != nil) != tc.hasUsage {
				t.Fatalf("trace=%+v", trace)
			}
			if tc.hasUsage {
				u := trace.Usage
				if u.PromptTokens != tc.prompt || u.CachedTokens != tc.cached || u.CacheUsageReported != tc.reported || u.CacheReportedPromptTokens != tc.prompt {
					t.Fatalf("usage=%+v", u)
				}
				if !tc.wantError && (usage.CacheUsageReported != tc.reported || usage.CacheReportedPromptTokens != tc.prompt) {
					t.Fatalf("returned usage=%+v", usage)
				}
			}
		})
	}
}
