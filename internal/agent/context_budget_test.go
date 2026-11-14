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
