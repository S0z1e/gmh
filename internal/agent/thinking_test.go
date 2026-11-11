package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

// Thinking streams as its own events: one thinking_delta per
// reasoning fragment, then thinking_end carrying the full text, ahead
// of the step usage and done. The final text is the answer only.
func TestRunEmitsThinkingEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"Let me "}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"think."}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"answer"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "answer" {
		t.Fatalf("final text = %q, want answer (thinking must not enter it)", text)
	}
	wantKinds := []EventKind{
		EventThinkingDelta, EventThinkingDelta, EventTextDelta,
		EventThinkingEnd, EventStepUsage, EventDone,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantKinds), events)
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Fatalf("event %d kind = %s, want %s", i, events[i].Kind, want)
		}
	}
	if events[0].Text != "Let me " || events[1].Text != "think." {
		t.Fatalf("thinking deltas = %q %q, want %q %q", events[0].Text, events[1].Text, "Let me ", "think.")
	}
	if events[3].Text != "Let me think." {
		t.Fatalf("thinking_end text = %q, want the full thinking", events[3].Text)
	}
	if events[5].Text != "answer" {
		t.Fatalf("done text = %q, want answer only", events[5].Text)
	}
}

// A turn with no thinking emits no thinking events at all — no empty
// thinking_end, no thinking_delta.
func TestRunEmptyThinkingEmitsNoEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"answer"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	if _, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == EventThinkingDelta || e.Kind == EventThinkingEnd {
			t.Fatalf("unexpected thinking event %+v in a turn with no thinking", e)
		}
	}
}

// Thinking is display-only: it never reaches the session file. The
// stored assistant message is the answer, and no stored message
// contains the thinking text.
func TestRunThinkingNotPersistedInSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"Let me think."}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"answer"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// session.New appends to an existing file for an explicit id, so
	// use a unique one: a stale file would make this test see an
	// older turn's messages.
	id := fmt.Sprintf("thinking-test-%d", time.Now().UnixNano())
	store, err := session.New(id)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
		Session:  store,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "answer" {
		t.Fatalf("final text = %q, want answer", text)
	}
	msgs, err := session.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "Let me think.") {
			t.Fatalf("session message contains thinking: %+v", m)
		}
	}
	found := false
	for _, m := range msgs {
		if m.Role == "assistant" && m.Content == "answer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("session has no assistant message with the answer: %+v", msgs)
	}
}
