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

func TestToAnthropicMessages(t *testing.T) {
	system, msgs := toAnthropicMessages([]Message{
		{Role: "system", Content: "be brief"},
		{Role: "system", Content: "second line"},
		{Role: "user", Content: "read x"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{"path":"x"}`}},
			{ID: "c2", Type: "function", Function: FunctionCall{Name: "grep", Arguments: `{"pattern":"y"}`}},
		}},
		{Role: "tool", Content: "hello", ToolCallID: "c1"},
		{Role: "tool", Content: "", ToolCallID: "c2"},
		{Role: "user", Content: "thanks"},
	})
	if system != "be brief\nsecond line" {
		t.Fatalf("system = %q", system)
	}
	// user, assistant(tool_use x2), user(tool_result x2 + text):
	// the two tool results and the following user text merge into
	// one user message, so alternation holds.
	if len(msgs) != 3 {
		t.Fatalf("msgs len = %d, want 3", len(msgs))
	}
	if msgs[0].Role != "user" || len(msgs[0].Content) != 1 || msgs[0].Content[0].Text != "read x" {
		t.Fatalf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || len(msgs[1].Content) != 2 {
		t.Fatalf("msgs[1] = %+v, want 2 tool_use blocks", msgs[1])
	}
	if msgs[1].Content[0].Type != "tool_use" || msgs[1].Content[0].ID != "c1" || msgs[1].Content[0].Name != "read" {
		t.Fatalf("msgs[1][0] = %+v", msgs[1].Content[0])
	}
	if string(msgs[1].Content[0].Input) != `{"path":"x"}` {
		t.Fatalf("msgs[1][0].Input = %s", msgs[1].Content[0].Input)
	}
	if msgs[2].Role != "user" || len(msgs[2].Content) != 3 {
		t.Fatalf("msgs[2] = %+v, want 2 tool_results + 1 text", msgs[2])
	}
	if msgs[2].Content[0].Type != "tool_result" || msgs[2].Content[0].ToolUseID != "c1" || msgs[2].Content[0].Content != "hello" {
		t.Fatalf("msgs[2][0] = %+v", msgs[2].Content[0])
	}
	// An empty tool result still carries content, so the block is valid.
	if msgs[2].Content[1].ToolUseID != "c2" || msgs[2].Content[1].Content != "(no output)" {
		t.Fatalf("msgs[2][1] = %+v, want tool_result c2 with (no output)", msgs[2].Content[1])
	}
	if msgs[2].Content[2].Type != "text" || msgs[2].Content[2].Text != "thanks" {
		t.Fatalf("msgs[2][2] = %+v", msgs[2].Content[2])
	}
}

// An assistant message carrying both text and a tool call keeps both,
// in order, in a single assistant message.
func TestToAnthropicMessagesAssistantTextAndToolUse(t *testing.T) {
	_, msgs := toAnthropicMessages([]Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "reading now", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{"path":"x"}`}},
		}},
	})
	if len(msgs) != 2 || len(msgs[1].Content) != 2 {
		t.Fatalf("msgs = %+v, want user + assistant with 2 blocks", msgs)
	}
	if msgs[1].Content[0].Type != "text" || msgs[1].Content[0].Text != "reading now" {
		t.Fatalf("msgs[1][0] = %+v", msgs[1].Content[0])
	}
	if msgs[1].Content[1].Type != "tool_use" || msgs[1].Content[1].Name != "read" {
		t.Fatalf("msgs[1][1] = %+v", msgs[1].Content[1])
	}
}

// A tool call with no arguments replays as an empty object, never as
// an empty string (which is not valid JSON for the tool input).
func TestToAnthropicMessagesEmptyArgs(t *testing.T) {
	_, msgs := toAnthropicMessages([]Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FunctionCall{Name: "bash", Arguments: ""}},
		}},
	})
	if string(msgs[1].Content[0].Input) != "{}" {
		t.Fatalf("Input = %s, want {}", msgs[1].Content[0].Input)
	}
}

func TestAnthropicStreamTextAndThinking(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		// The exact event sequence probed live from Discovery.
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":15,\"cache_read_input_tokens\":5,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reasoning\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"\\n\\n\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic"}
	var got Message
	var gotUsage Usage
	var streamed string
	err := c.Stream(context.Background(), []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}, nil, func(s string) { streamed += s }, func(m Message, u Usage) { got = m; gotUsage = u }, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Thinking is skipped; only the text reaches the caller.
	if got.Content != "\n\nHi" {
		t.Fatalf("content = %q, want \"\\n\\nHi\"", got.Content)
	}
	if streamed != "\n\nHi" {
		t.Fatalf("streamed = %q", streamed)
	}
	if len(got.ToolCalls) != 0 {
		t.Fatalf("want no tool calls, got %+v", got.ToolCalls)
	}
	if gotUsage.PromptTokens != 15 || gotUsage.CachedTokens != 5 || gotUsage.CompletionTokens != 20 || gotUsage.TotalTokens != 35 {
		t.Fatalf("usage = %+v, want prompt 15 cached 5 completion 20 total 35", gotUsage)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	// A non-Anthropic host authenticates with Bearer only: sending
	// x-api-key as well breaks Discovery, which prefers it and then
	// rejects the unexchanged surrogate (verified live 2026-11-09).
	if gotKey != "" {
		t.Fatalf("x-api-key = %q, want it absent on a Bearer host", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", gotVersion)
	}
	if gotBody["model"] != "test-model" || gotBody["max_tokens"] != float64(16384) || gotBody["stream"] != true {
		t.Fatalf("body = %+v, want model test-model, max_tokens 16384, stream true", gotBody)
	}
	if gotBody["system"] != "sys" {
		t.Fatalf("system = %v", gotBody["system"])
	}
}

func TestAnthropicStreamFragmentedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"read\",\"input\":{}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"README.md\\\"}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":8}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic"}
	var got Message
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "read it"}}, nil, nil, func(m Message, u Usage) { got = m }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].ID != "call_1" || got.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("call = %+v", got.ToolCalls[0])
	}
	if want := `{"path":"README.md"}`; got.ToolCalls[0].Function.Arguments != want {
		t.Fatalf("arguments = %q, want %q", got.ToolCalls[0].Function.Arguments, want)
	}
}

func TestAnthropicStreamTwoToolCallsInOneTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"read\",\"input\":{}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_2\",\"name\":\"write\",\"input\":{}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"b.txt\\\"}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic"}
	var got Message
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "do two things"}}, nil, nil, func(m Message, u Usage) { got = m }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %+v", len(got.ToolCalls), got.ToolCalls)
	}
	if got.ToolCalls[0].Function.Name != "read" || got.ToolCalls[1].Function.Name != "write" {
		t.Fatalf("wrong calls: %+v", got.ToolCalls)
	}
	if got.ToolCalls[0].ID != "call_1" || got.ToolCalls[1].ID != "call_2" {
		t.Fatalf("wrong call ids: %+v", got.ToolCalls)
	}
}

// A stream that ends without message_stop was cut off mid-reply: fail
// loudly instead of running a half-accumulated call.
func TestAnthropicStreamRequiresMessageStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"half a\"}}\n\n")
		// No message_stop; the handler just returns.
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic"}
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {}, nil)
	if err == nil || !strings.Contains(err.Error(), "without message_stop") {
		t.Fatalf("err = %v, want a missing-message_stop error", err)
	}
}

func TestAnthropicBearerAuthHostSelection(t *testing.T) {
	cases := []struct {
		base string
		want bool // true = Bearer, false = x-api-key
	}{
		{"https://discovery-api.intern-ai.org.cn/v1", true},
		{"https://discovery-api.intern-ai.org.cn", true},
		{"https://api.anthropic.com", false},
		{"https://api.anthropic.com/v1", false},
		{"https://example.com/v1", true},
	}
	for _, tc := range cases {
		if got := anthropicBearerAuth(tc.base); got != tc.want {
			t.Errorf("anthropicBearerAuth(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

func TestAnthropicEndpointNormalisation(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://discovery-api.intern-ai.org.cn/v1", "https://discovery-api.intern-ai.org.cn/v1/messages"},
		{"https://discovery-api.intern-ai.org.cn", "https://discovery-api.intern-ai.org.cn/v1/messages"},
		{"https://discovery-api.intern-ai.org.cn/v1/messages", "https://discovery-api.intern-ai.org.cn/v1/messages"},
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
	}
	for _, tc := range cases {
		c := &Client{BaseURL: tc.base, API: "anthropic"}
		if got := c.endpoint(); got != tc.want {
			t.Errorf("endpoint(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
	if c := (&Client{API: "anthropic"}); c.Mode() != "anthropic" {
		t.Errorf("Mode() = %q, want anthropic", c.Mode())
	}
	if c := (&Client{BaseURL: "https://discovery-api.intern-ai.org.cn/v1"}); c.ProviderName() != "Discovery" {
		t.Errorf("ProviderName() = %q, want Discovery", c.ProviderName())
	}
	if c := (&Client{BaseURL: "https://www.openconsole.cn/v1"}); c.ProviderName() != "OpenConsole" {
		t.Errorf("ProviderName() = %q, want OpenConsole", c.ProviderName())
	}
	if c := (&Client{BaseURL: "https://example.com/v1"}); c.ProviderName() != "example.com" {
		t.Errorf("ProviderName() = %q, want example.com", c.ProviderName())
	}
}

// SamplingParams apply only to OpenAI-compatible APIs:
// the Anthropic body must not carry them.
func TestAnthropicIgnoresSamplingParams(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":0,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c := &Client{
		BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic",
		SamplingParams: map[string]any{"temperature": 0.7},
	}
	if err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["temperature"]; ok {
		t.Fatalf("temperature must not be sent to Anthropic: %+v", body)
	}
	if body["max_tokens"] != float64(16384) {
		t.Fatalf("max_tokens = %v, want 16384", body["max_tokens"])
	}
}

func TestAnthropicSendsThinkingBudget(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":0,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic", MaxTokens: 16384, Effort: "high"}
	if err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {}, nil); err != nil {
		t.Fatal(err)
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8192) {
		t.Fatalf("thinking = %v, want {type: enabled, budget_tokens: 8192}: %+v", body["thinking"], body)
	}

	// Unset effort omits the field entirely.
	body = nil
	c.Effort = ""
	if err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatalf("thinking must be absent without effort: %+v", body)
	}
}

func TestEffortThinkingBudget(t *testing.T) {
	cases := []struct {
		effort    string
		maxTokens int
		want      int
	}{
		{"low", 16384, 1024},
		{"medium", 16384, 4096},
		{"high", 16384, 8192},
		{"low", 100000, 6250},
		{"medium", 100000, 25000},
		{"high", 100000, 50000},
		{"low", 2048, 1024},  // floor: 2048/16 = 128 < 1024
		{"high", 2048, 1024}, // 2048/2 = 1024, still below maxTokens
		{"low", 1024, 1023},  // degenerate: floor 1024 then capped below maxTokens
		{"", 16384, 0},
		{"ultra", 16384, 0},
	}
	for _, tc := range cases {
		if got := effortThinkingBudget(tc.effort, tc.maxTokens); got != tc.want {
			t.Fatalf("effortThinkingBudget(%q, %d) = %d, want %d", tc.effort, tc.maxTokens, got, tc.want)
		}
	}
}
