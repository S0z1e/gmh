package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
