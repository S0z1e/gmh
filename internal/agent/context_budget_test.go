package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompactsInsideToolLoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("loop-compact")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f"), []byte("hello"), 0600)
	calls, compacts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{\"path\":\"f\"}"}}]}}]}`+"\n\n")
		} else {
			if compacts != 1 {
				t.Error("next LLM request happened before compaction")
			}
			var request struct {
				Messages []llm.Message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "user" || last.Content != "read f" {
				t.Errorf("active task lost: %+v", last)
			}
			saved, err := session.Load(store.ID)
			if err != nil || len(saved) != 2 || saved[1].Content != "read f" {
				t.Errorf("checkpoint = %+v, %v", saved, err)
			}
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"}}]}`+"\n\n")
		}
		fmt.Fprint(w, `data: {"usage":{"prompt_tokens":8000,"completion_tokens":1,"total_tokens":8001}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "mock"}, Exec: &tools.Executor{Root: root}, Tools: ToolDefs(), Mode: "auto", ContextLimit: 10000, CompactHistory: func(ctx context.Context, h []llm.Message) (string, llm.Usage, error) {
		compacts++
		return "summary", llm.Usage{TotalTokens: 5}, nil
	}}
	cfg.Session = store
	_, u, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Kind: "task", Content: "read f"}})
	if err != nil || calls != 2 || compacts != 1 || u.TotalTokens != 16007 {
		t.Fatalf("err=%v calls=%d compacts=%d usage=%+v", err, calls, compacts, u)
	}
}

func TestContextBudgetIncludesNewResults(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: "task"}}
	cfg := &Config{ContextLimit: 10000, LastPromptTokens: 6500}
	cfg.lastPromptEstimate = estimatePromptTokens(msgs, nil)
	msgs = append(msgs, llm.Message{Role: "assistant", Content: strings.Repeat("x", 1800)})
	if !cfg.needsCompact(msgs) {
		t.Fatal("new content did not count toward next request")
	}
}

func TestFailedCompactionPreservesHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := session.New("failed-compact")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task := llm.Message{Role: "user", Kind: "task", Content: "keep this task"}
	if err := s.Append(task); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Session: s, CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
		return "", llm.Usage{}, errors.New("provider failed")
	}}
	if _, _, err := cfg.compactInLoop(context.Background(), []llm.Message{task}); err == nil {
		t.Fatal("failed compact succeeded")
	}
	saved, err := session.Load(s.ID)
	if err != nil || len(saved) != 1 || saved[0].Content != task.Content {
		t.Fatalf("history = %+v, %v", saved, err)
	}
}

func TestRunCompactionRetriesTransientFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("compact-retry")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := llm.Message{Role: "user", Kind: "task", Content: "finish verification"}
	if err := store.Append(task); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "RBAC: access denied")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 2 {
			fmt.Fprint(w, textSSE("verification remains pending"))
		} else {
			fmt.Fprint(w, textSSE("verification complete"))
		}
	}))
	defer srv.Close()
	var events []Event
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "mock"}, Session: store, ContextLimit: 10000, LastPromptTokens: 8000, OnEvent: func(e Event) { events = append(events, e) }}
	text, _, err := cfg.Run(context.Background(), []llm.Message{task})
	if err != nil || text != "verification complete" || calls.Load() != 3 {
		t.Fatalf("Run = %q, %v; calls = %d; transient compaction failure ended the turn", text, err, calls.Load())
	}
	if countKind(events, EventStepRetry) != 1 {
		t.Fatal("compaction retry was not observable")
	}
	saved, err := session.Load(store.ID)
	if err != nil || len(saved) != 3 || saved[0].Kind != "summary" || saved[1].Content != task.Content {
		t.Fatalf("recovered checkpoint = %+v, %v", saved, err)
	}
}

func TestRunCompactionFailureReportsStopWithoutLosingHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("compact-unavailable")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task := llm.Message{Role: "user", Kind: "task", Content: "verify the build"}
	if err := store.Append(task); err != nil {
		t.Fatal(err)
	}
	var events []Event
	cfg := &Config{Session: store, ContextLimit: 10000, LastPromptTokens: 8000, CompactHistory: func(context.Context, []llm.Message) (string, llm.Usage, error) {
		return "", llm.Usage{}, errors.New("provider unavailable")
	}, OnEvent: func(e Event) { events = append(events, e) }}
	text, _, err := cfg.Run(context.Background(), []llm.Message{task})
	if err == nil || !strings.Contains(text, "provider unavailable") || !strings.Contains(text, "stopped") {
		t.Fatalf("failure report = %q, %v", text, err)
	}
	if countKind(events, EventError) != 1 || events[len(events)-1].Text != text {
		t.Fatalf("terminal events = %+v", events)
	}
	saved, err := session.Load(store.ID)
	if err != nil || len(saved) != 1 || saved[0].Content != task.Content {
		t.Fatalf("failed compaction changed history: %+v, %v", saved, err)
	}
}

func TestRunCancelledCompactionReportsAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, calls := retryTestServer(t, 100, http.StatusForbidden, "1")
	var events []Event
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "mock"}, ContextLimit: 10000, LastPromptTokens: 8000, OnEvent: func(e Event) {
		events = append(events, e)
		if e.Kind == EventStepRetry {
			cancel()
		}
	}}
	text, _, err := cfg.Run(ctx, []llm.Message{{Role: "user", Content: "verify the build"}})
	if err == nil || cfg.LastTerminalReason != TerminalAbort || text != "" {
		t.Fatalf("cancelled compaction = %q, %v, %s; want abort without a provider failure report", text, err, cfg.LastTerminalReason)
	}
	if atomic.LoadInt32(calls) != 1 || events[len(events)-1].TerminalReason != TerminalAbort {
		t.Fatalf("cancelled compaction calls = %d, events = %+v", atomic.LoadInt32(calls), events)
	}
}
