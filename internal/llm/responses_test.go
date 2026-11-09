package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
