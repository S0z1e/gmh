package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesStreamAccumulatesFragmentedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_1\",\"call_id\":\"call_1\",\"name\":\"read\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"{\\\"path\\\":\\\"\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"README.md\\\"}\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "responses"}
	var got Message
	var gotUsage Usage
	err := c.Stream(context.Background(), []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}, nil, nil, func(m Message, u Usage) { got = m; gotUsage = u })
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "hi" {
		t.Fatalf("content = %q, want hi", got.Content)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("want read, got %s", got.ToolCalls[0].Function.Name)
	}
	if want := `{"path":"README.md"}`; got.ToolCalls[0].Function.Arguments != want {
		t.Fatalf("arguments = %q, want %q", got.ToolCalls[0].Function.Arguments, want)
	}
	if gotUsage.TotalTokens != 15 || gotUsage.PromptTokens != 10 {
		t.Fatalf("usage = %+v, want total 15 / prompt 10", gotUsage)
	}
}

// Two function calls in one Responses turn: both must survive, in
// order, with no duplicates when response.completed repeats them.
// (Proven live against OpenConsole 2026-11-09: one turn emitted two
// calls and both were recorded in the session.)
func TestResponsesStreamTwoToolCallsInOneTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_1\",\"call_id\":\"call_1\",\"name\":\"read\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_2\",\"call_id\":\"call_2\",\"name\":\"write\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_2\",\"delta\":\"{\\\"path\\\":\\\"b.txt\\\"}\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"},{\"type\":\"function_call\",\"call_id\":\"call_2\",\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"b.txt\\\"}\"}],\"usage\":{\"input_tokens\":20,\"output_tokens\":10,\"total_tokens\":30,\"input_tokens_details\":{\"cached_tokens\":7}}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "responses"}
	var got Message
	var gotUsage Usage
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "do two things"}}, nil, nil, func(m Message, u Usage) { got = m; gotUsage = u })
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %+v", len(got.ToolCalls), got.ToolCalls)
	}
	if got.ToolCalls[0].Function.Name != "read" || got.ToolCalls[1].Function.Name != "write" {
		t.Fatalf("wrong call order/names: %+v", got.ToolCalls)
	}
	if got.ToolCalls[0].ID != "call_1" || got.ToolCalls[1].ID != "call_2" {
		t.Fatalf("wrong call ids: %+v", got.ToolCalls)
	}
	if gotUsage.CachedTokens != 7 || gotUsage.TotalTokens != 30 {
		t.Fatalf("usage = %+v, want cached 7 / total 30", gotUsage)
	}
}

// A denied tool call must replay as a function_call_output carrying the
// denial text and the SAME call_id, so the next Responses request keeps
// every function_call paired (an unpaired call is the replay bug class).
func TestToResponsesInputDeniedToolResult(t *testing.T) {
	_, input := toResponsesInput([]Message{
		{Role: "user", Content: "write x"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c9", Type: "function", Function: FunctionCall{Name: "write", Arguments: `{"path":"x"}`}}}},
		{Role: "tool", Content: "User denied this tool call. Do not retry it.", ToolCallID: "c9"},
	})
	if len(input) != 3 {
		t.Fatalf("input len = %d, want 3", len(input))
	}
	call, ok := input[1].(map[string]any)
	if !ok || call["type"] != "function_call" || call["call_id"] != "c9" {
		t.Fatalf("input[1] = %#v, want function_call c9", input[1])
	}
	out, ok := input[2].(map[string]any)
	if !ok || out["type"] != "function_call_output" || out["call_id"] != "c9" || out["output"] != "User denied this tool call. Do not retry it." {
		t.Fatalf("input[2] = %#v, want function_call_output c9 with the denial", input[2])
	}
}

// A Responses stream that ends without response.completed was cut off
// mid-reply: fail loudly instead of running a half-accumulated call.
func TestResponsesStreamRequiresCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"half a\"}\n\n")
		// No response.completed; the handler just returns.
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "responses"}
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {})
	if err == nil || !strings.Contains(err.Error(), "without response.completed") {
		t.Fatalf("err = %v, want a missing-completed error", err)
	}
}

// When response.completed lists only one of two calls the stream
// carried, the other must still survive, merged from the fragments.
func TestResponsesStreamMergesPartialCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_1\",\"call_id\":\"call_1\",\"name\":\"read\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_2\",\"call_id\":\"call_2\",\"name\":\"write\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_2\",\"delta\":\"{\\\"path\\\":\\\"b.txt\\\"}\"}\n\n")
		// completed lists only the first call.
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}],\"usage\":{\"input_tokens\":20,\"output_tokens\":10,\"total_tokens\":30}}}\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "responses"}
	var got Message
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "do two things"}}, nil, nil, func(m Message, u Usage) { got = m })
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %+v", len(got.ToolCalls), got.ToolCalls)
	}
	if got.ToolCalls[0].Function.Name != "read" || got.ToolCalls[1].Function.Name != "write" {
		t.Fatalf("wrong calls: %+v", got.ToolCalls)
	}
}

func TestToResponsesInput(t *testing.T) {
	instructions, input := toResponsesInput([]Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "read x"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{"path":"x"}`}}}},
		{Role: "tool", Content: "hello", ToolCallID: "c1"},
	})
	if instructions != "be brief" {
		t.Fatalf("instructions = %q", instructions)
	}
	if len(input) != 3 {
		t.Fatalf("input len = %d, want 3 (user, function_call, function_call_output)", len(input))
	}
}

func TestEndpointNormalisationBothModes(t *testing.T) {
	for _, mode := range []string{"chat", "responses"} {
		c := &Client{BaseURL: "https://www.openconsole.cn", API: mode}
		want := "https://www.openconsole.cn/v1/chat/completions"
		if mode == "responses" {
			want = "https://www.openconsole.cn/v1/responses"
		}
		if got := c.endpoint(); got != want {
			t.Errorf("endpoint(%s) = %q, want %q", mode, got, want)
		}
	}
	// Default mode is chat.
	c := &Client{BaseURL: "https://www.openconsole.cn"}
	if c.Mode() != "chat" {
		t.Errorf("default mode = %q, want chat", c.Mode())
	}
}

// SamplingParams and max_output_tokens merge into the Responses
// request body too.
func TestResponsesMergesSamplingParamsAndMaxOutputTokens(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{
		BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "responses",
		MaxTokens:      16384,
		SamplingParams: map[string]any{"temperature": 0.7},
	}
	if err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {}); err != nil {
		t.Fatal(err)
	}
	if body["temperature"] != 0.7 {
		t.Fatalf("temperature missing: %+v", body)
	}
	if body["max_output_tokens"] != float64(16384) {
		t.Fatalf("max_output_tokens = %v, want 16384", body["max_output_tokens"])
	}
}
