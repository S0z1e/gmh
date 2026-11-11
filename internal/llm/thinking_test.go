package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Chat: reasoning_content deltas reach onThinking in order, never
// enter the answer, and the usage details surface reasoning tokens.
func TestStreamChatForwardsReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"Thinking "}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"hard"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"answer"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":10,"total_tokens":22,"completion_tokens_details":{"reasoning_tokens":7}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"}
	var got Message
	var gotUsage Usage
	var thinking strings.Builder
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
		func(m Message, u Usage) { got = m; gotUsage = u },
		func(s string) { thinking.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if thinking.String() != "Thinking hard" {
		t.Fatalf("thinking = %q, want %q", thinking.String(), "Thinking hard")
	}
	if got.Content != "answer" {
		t.Fatalf("content = %q, want answer (reasoning must not enter it)", got.Content)
	}
	if gotUsage.ReasoningTokens != 7 {
		t.Fatalf("reasoning tokens = %d, want 7", gotUsage.ReasoningTokens)
	}
}

// Chat: a nil onThinking must not panic when reasoning arrives; the
// answer is still correct. (The loop always passes one, but the
// client must be safe without it.)
func TestStreamChatNilOnThinkingIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"secret thought"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"}
	var got Message
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
		func(m Message, u Usage) { got = m }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" {
		t.Fatalf("content = %q, want ok", got.Content)
	}
}

// Responses: reasoning summary/text deltas reach onThinking in order;
// a reasoning item whose summary never streamed is backfilled once
// from its output_item.done event; an item already seen as deltas is
// never repeated, even when response.completed repeats it.
func TestResponsesReasoningDeltasAndBackfill(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"First thought"}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_summary_text","text":"First thought"}]}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_2"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_2","content":[{"type":"reasoning_summary_text","text":"Second thought"}]}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"answer"}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_summary_text","text":"First thought"}]},{"type":"reasoning","id":"rs_2","content":[{"type":"reasoning_summary_text","text":"Second thought"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"output_tokens_details":{"reasoning_tokens":4}}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "responses"}
	var got Message
	var gotUsage Usage
	var thinking []string
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
		func(m Message, u Usage) { got = m; gotUsage = u },
		func(s string) { thinking = append(thinking, s) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"First thought", "Second thought"}; !equalStrings(thinking, want) {
		t.Fatalf("thinking = %q, want %q", thinking, want)
	}
	if got.Content != "answer" {
		t.Fatalf("content = %q, want answer", got.Content)
	}
	if gotUsage.ReasoningTokens != 4 {
		t.Fatalf("reasoning tokens = %d, want 4", gotUsage.ReasoningTokens)
	}
}

// Responses: encrypted_content is opaque — a reasoning item whose
// content is only encrypted, or a mixed item, forwards only the
// readable text parts, never the encrypted blob.
func TestResponsesReasoningEncryptedContentNeverForwarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_9","content":[{"type":"reasoning.encrypted_content","encrypted_content":"opaque-blob-1"}]}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_10","content":[{"type":"reasoning_summary_text","text":"Real summary"},{"type":"reasoning.encrypted_content","encrypted_content":"opaque-blob-2"}]}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"ok"}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "responses"}
	var thinking []string
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
		func(m Message, u Usage) {},
		func(s string) { thinking = append(thinking, s) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Real summary"}; !equalStrings(thinking, want) {
		t.Fatalf("thinking = %q, want %q (encrypted_content must never reach onThinking)", thinking, want)
	}
}

// Anthropic: thinking_delta reaches onThinking; signature_delta is
// accumulated, never forwarded; the signature from content_block_start
// is never forwarded either.
func TestStreamAnthropicThinkingDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","signature":"sig-abc"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think."}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-def"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_stop","index":1}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic"}
	var got Message
	var thinking strings.Builder
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
		func(m Message, u Usage) { got = m },
		func(s string) { thinking.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if thinking.String() != "Let me think." {
		t.Fatalf("thinking = %q, want %q", thinking.String(), "Let me think.")
	}
	if strings.Contains(thinking.String(), "sig-") {
		t.Fatalf("thinking contains a signature: %q", thinking.String())
	}
	if got.Content != "Hi" {
		t.Fatalf("content = %q, want Hi (thinking must not enter it)", got.Content)
	}
}

// Anthropic: a redacted_thinking block has no text — it emits nothing.
func TestStreamAnthropicRedactedThinkingIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_stop","index":1}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "anthropic"}
	var got Message
	var thinking []string
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
		func(m Message, u Usage) { got = m },
		func(s string) { thinking = append(thinking, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(thinking) != 0 {
		t.Fatalf("thinking = %q, want none (redacted_thinking emits nothing)", thinking)
	}
	if got.Content != "ok" {
		t.Fatalf("content = %q, want ok", got.Content)
	}
}

// Usage: reasoning tokens accumulate in Add and render in String only
// when non-zero. They are a subset of completion — String never adds
// them to any total.
func TestUsageReasoningTokens(t *testing.T) {
	var u Usage
	u.Add(Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18, ReasoningTokens: 5})
	u.Add(Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6, ReasoningTokens: 2})
	if u.ReasoningTokens != 7 {
		t.Fatalf("reasoning = %d, want 7", u.ReasoningTokens)
	}
	if s := u.String(); !strings.Contains(s, "reasoning=7") || !strings.Contains(s, "completion=10") || !strings.Contains(s, "total=24") {
		t.Fatalf("String() = %q, want reasoning=7, completion=10, total=24", s)
	}
	if s := (Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}).String(); strings.Contains(s, "reasoning") {
		t.Fatalf("String() = %q, want no reasoning figure when zero", s)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
