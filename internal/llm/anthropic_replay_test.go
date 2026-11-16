package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestAnthropicPreservesReplayBlocksAndAdaptiveThinking(t *testing.T) {
	var req map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"first\",\"signature\":\"sig-\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\" rest\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"end\"}}\n\n")
		fmt.Fprint(w, protocolEvent(map[string]any{"type": "content_block_stop", "index": 0}))
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"opaque\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"read\",\"input\":{}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"x\\\"}\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "fake", Model: "claude-opus-4-7", API: "anthropic", Effort: "high"}
	msg, _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "task"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req["thinking"].(map[string]any)["type"] != "adaptive" || req["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("bad thinking format: %v", req["thinking"])
	}
	if len(msg.AnthropicContent) != 3 || msg.AnthropicContent[0].Thinking != "first rest" || msg.AnthropicContent[0].Signature != "sig-end" || msg.AnthropicContent[1].Data != "opaque" || len(msg.ToolCalls) != 1 {
		t.Fatalf("replay lost: %+v", msg)
	}
	encoded, _ := json.Marshal(msg)
	var resumed Message
	if err := json.Unmarshal(encoded, &resumed); err != nil {
		t.Fatal(err)
	}
	_, messages := toAnthropicMessages([]Message{resumed, {Role: "tool", ToolCallID: "t1", Content: "result"}})
	if !reflect.DeepEqual(messages[0].Content, msg.AnthropicContent) {
		t.Fatal("blocks changed after persistence and replay")
	}
	for model, want := range map[string]string{"Agents-A1": "enabled", "claude-3-5-sonnet-20241022": "enabled", "claude-3-7-sonnet-latest": "enabled", "claude-sonnet-4-5": "enabled", "claude-opus-4-6": "adaptive", "claude-opus-4-7": "adaptive", "claude-sonnet-5-5": "adaptive"} {
		if got := anthropicThinkingMode(model); got != want {
			t.Fatalf("%s=%s", model, got)
		}
	}
	c.Thinking = "enabled"
	c.MaxTokens = 512
	if _, _, err := c.Chat(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("invalid manual thinking budget accepted")
	}
}

func TestAnthropicReplaySkipsEmptyText(t *testing.T) {
	_, messages := toAnthropicMessages([]Message{{Role: "user", Content: "task"}, {Role: "assistant", AnthropicContent: []AnthropicContentBlock{{Type: "text"}}}, {Role: "user", Content: "resume"}})
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("empty replay retained: %+v", messages)
	}
}

func TestAnthropicReplayHonorsContextRewrites(t *testing.T) {
	m := Message{Role: "assistant", Content: "redacted", AnthropicContent: []AnthropicContentBlock{{Type: "thinking", Thinking: "reasoning", Signature: "opaque"}, {Type: "text", Text: "secret"}, {Type: "tool_use", ID: "old", Name: "write", Input: json.RawMessage(`{"content":"secret"}`)}}}
	blocks := anthropicReplayBlocks(m)
	if len(blocks) != 2 || blocks[0].Signature != "opaque" || blocks[1].Text != "redacted" {
		t.Fatalf("context redaction ignored: %+v", blocks)
	}
	m.ToolCalls = []ToolCall{{ID: "old", Function: FunctionCall{Name: "read", Arguments: `{"path":"public"}`}}}
	blocks = anthropicReplayBlocks(m)
	if len(blocks) != 3 || blocks[2].Name != "read" || string(blocks[2].Input) != `{"path":"public"}` {
		t.Fatalf("context tool rewrite ignored: %+v", blocks)
	}
}

func TestAnthropicRejectsIncompleteToolMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"input\":{}}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "fake", API: "anthropic", Model: "test"}
	if _, _, err := c.Chat(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("malformed tool block accepted")
	}
}
