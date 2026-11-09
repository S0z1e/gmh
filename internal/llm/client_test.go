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

// The provider splits tool-call arguments across SSE deltas; the client
// must reassemble them exactly, or the tool JSON will not parse.
func TestStreamAccumulatesFragmentedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("wrong auth: %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Two fragments of the same tool call, index 0.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"README.md\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"}
	var got Message
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) { got = m })
	if err != nil {
		t.Fatal(err)
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
}

// The client must ask for usage in the streaming Chat Completions
// request (stream_options.include_usage), or a spec-compliant
// provider sends no usage object at all and the turn reports 0
// tokens. The final usage-only chunk (empty choices) must be picked
// up by the parser.
func TestStreamChatRequestsUsage(t *testing.T) {
	var body struct {
		StreamOptions *streamOptions `json:"stream_options"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		// Usage-only final chunk, as OpenAI sends with include_usage.
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"}
	var gotUsage Usage
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) { gotUsage = u })
	if err != nil {
		t.Fatal(err)
	}
	if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options = %+v, want {include_usage:true}", body.StreamOptions)
	}
	if gotUsage.PromptTokens != 12 || gotUsage.CompletionTokens != 3 || gotUsage.TotalTokens != 15 || gotUsage.CachedTokens != 4 {
		t.Fatalf("usage = %+v, want prompt 12 (cached 4) completion 3 total 15", gotUsage)
	}
}

// A stream that ends without [DONE] was cut off mid-reply: fail
// loudly instead of returning a half-accumulated tool call as if it
// were complete.
func TestStreamChatRequiresDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half a\"}}]}\n\n")
		// No [DONE]; the handler just returns, closing the stream.
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"}
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {})
	if err == nil || !strings.Contains(err.Error(), "without [DONE]") {
		t.Fatalf("err = %v, want a missing-[DONE] error", err)
	}
}

func TestEndpointNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.openconsole.cn":    "https://www.openconsole.cn/v1/chat/completions",
		"https://www.openconsole.cn/v1": "https://www.openconsole.cn/v1/chat/completions",
		"https://api.openai.com/v1":     "https://api.openai.com/v1/chat/completions",
		"https://x.example.com/v1/":     "https://x.example.com/v1/chat/completions",
	} {
		c := &Client{BaseURL: in}
		if got := c.endpoint(); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
