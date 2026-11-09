package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/agent"
)

func mockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi there\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestRunnerHandleTaskAndUsage(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var events []agent.Event
	a.Cfg.OnEvent = func(e agent.Event) { events = append(events, e) }
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	if quit := r.Handle(context.Background(), "say hi"); quit {
		t.Fatal("task should not quit")
	}
	if len(a.History) != 2 { // user + assistant
		t.Fatalf("history = %d messages, want 2", len(a.History))
	}
	if r.SessionUsage.TotalTokens != 15 {
		t.Fatalf("session usage total = %d, want 15", r.SessionUsage.TotalTokens)
	}
	joined := strings.Join(printed, "\n")
	if !strings.Contains(joined, "[usage] turn: prompt=10") {
		t.Fatalf("turn usage not printed: %v", printed)
	}
	if quit := r.Handle(context.Background(), "/usage"); quit {
		t.Fatal("/usage should not quit")
	}
	if !strings.Contains(strings.Join(printed, "\n"), "[usage] session: prompt=10") {
		t.Fatalf("session usage not printed: %v", printed)
	}
}

func TestRunnerHandleClear(t *testing.T) {
	srv := mockServer(t)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	r := &Runner{App: a, Print: func(format string, args ...any) {}}
	r.Handle(context.Background(), "say hi")
	if len(a.History) == 0 {
		t.Fatal("history should have messages before clear")
	}
	if quit := r.Handle(context.Background(), "/clear"); quit {
		t.Fatal("/clear should not quit")
	}
	if len(a.History) != 0 {
		t.Fatalf("history after /clear = %d, want 0", len(a.History))
	}
}

func TestRunnerHandleQuit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	a, err := Setup(Params{Root: t.TempDir(), Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	r := &Runner{App: a, Print: func(format string, args ...any) {}}
	if !r.Handle(context.Background(), "/quit") {
		t.Fatal("/quit should return true")
	}
	if r.Handle(context.Background(), "hello") {
		t.Fatal("a task should not quit")
	}
}
