package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderOutputLimitMetadata(t *testing.T) {
	cases := []struct{ api, stream, want string }{
		{"responses", `data: {"type":"response.output_text.delta","delta":"partial"}

data: {"type":"response.incomplete","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`, "incomplete"},
		{"anthropic", `event: message_start
data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":10}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`, "max_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.api, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.stream)
			}))
			defer srv.Close()
			client := &Client{API: tc.api, BaseURL: srv.URL, APIKey: "test", Model: "mock"}
			msg, usage, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "task"}}, nil, nil)
			if err != nil || msg.FinishReason != tc.want || msg.Content != "partial" || usage.TotalTokens != 15 {
				t.Fatalf("message=%+v usage=%+v error=%v", msg, usage, err)
			}
		})
	}
}

func TestAnthropicToolErrorMetadata(t *testing.T) {
	_, msgs := toAnthropicMessages([]Message{{Role: "tool", ToolCallID: "failed", Content: "failure", ToolError: true}})
	if len(msgs) != 1 || len(msgs[0].Content) != 1 || !msgs[0].Content[0].IsError {
		t.Fatalf("tool result lost error status: %+v", msgs)
	}
}
