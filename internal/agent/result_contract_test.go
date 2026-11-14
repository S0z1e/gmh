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

func TestBashFailureHasTypedError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"command\":\"exit 7\"}"}}]}}]}`+"\n\n")
		} else {
			fmt.Fprint(w, textSSE("done"))
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	var end Event
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, Model: "mock", APIKey: "test"}, Exec: &tools.Executor{Root: t.TempDir()}, Tools: ToolDefs(), Mode: "auto", OnEvent: func(e Event) {
		if e.Kind == EventToolEnd {
			end = e
		}
	}}
	cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "task"}})
	if end.ToolErr == nil || end.ToolStatus != tools.ResultError || end.ToolExitCode == nil || *end.ToolExitCode != 7 {
		t.Fatalf("bash exit 7 lost failure metadata: %+v", end)
	}
}
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
