package rpc

import (
	"fmt"
	"io"
	"minimalharness/internal/llm"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConcurrentQueriesUsePublishedState(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "ok"}})
	s := &Server{app: a, runner: runner, out: io.Discard, work: make(chan workItem, 1)}
	s.refreshState()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			s.handleQuery(Request{Type: "get_state"})
			s.handleQuery(Request{Type: "usage"})
			s.handleQuery(Request{Type: "list_sessions"})
		}
	}()
	id := a.Store.ID
	for i := 0; i < 100; i++ {
		a.Cfg.Client = &llm.Client{Model: fmt.Sprintf("model-%d", i)}
		a.Store.ID = fmt.Sprintf("session-%d", i)
		a.ContextLimit = 1000 + i
		s.refreshState()
	}
	<-done
	a.Store.ID = id
}

func TestReloadErrorKeepsCompletedRPCAnswer(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "unused"}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := os.OpenFile(a.Store.Path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = f.WriteString(strings.Repeat("x", 5<<20) + "\n")
		_ = f.Close()
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"completed answer"}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a.Cfg.Client.BaseURL = srv.URL
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "task"})
	d.waitForResponse("p1", 5*time.Second)
	settled := d.waitFor("agent_settled", 5*time.Second)
	if settled["text"] != "completed answer" || settled["reason"] != "error" || settled["error"] == nil {
		t.Fatalf("reload lost answer: %+v", settled)
	}
	d.quit(t)
}
