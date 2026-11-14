package agent

import (
	"context"
	"fmt"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOutputLimitIsNotCompletedTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "test"}}
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "task"}})
	if err == nil || cfg.LastTerminalReason == TerminalEndTurn {
		t.Fatal("truncated output reported as success")
	}
}

func TestSessionWriteFailureStopsBeforeToolSideEffect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("write-failure")
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, toolCallSSE("call_write", "write", `{"path":"unexpected","content":"bad"}`))
	}))
	defer srv.Close()
	root := t.TempDir()
	cfg := &Config{Session: store, Client: &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "test"}, Exec: &tools.Executor{Root: root}, Tools: ToolDefs(), Mode: "auto"}
	_, _, err = cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "task"}})
	if err == nil {
		t.Fatal("session write failure ignored")
	}
	if _, err := os.Stat(filepath.Join(root, "unexpected")); !os.IsNotExist(err) {
		t.Fatal("tool executed without a durable call record")
	}
}
