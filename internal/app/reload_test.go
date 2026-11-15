package app

import (
	"context"
	"minimalharness/internal/agent"
	"os"
	"strings"
	"testing"
)

func TestReloadFailureIsReportedAndPreventsStaleContinuation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "fake")
	root := t.TempDir()
	writeMockProvider(t, root)
	var bodies [][]byte
	srv := recordingServer(t, &bodies)
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	a, err := Setup(Params{Root: root, Mode: "auto", MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	a.Cfg.OnEvent = func(e agent.Event) {
		if e.Kind == agent.EventDone {
			f, err := os.OpenFile(a.Store.Path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = f.WriteString(strings.Repeat("x", 5<<20) + "\n")
			_ = f.Close()
		}
	}
	r := &Runner{App: a, Print: func(string, ...any) {}}
	r.Handle(context.Background(), "first task")
	if r.LastErr == nil || !strings.Contains(r.LastErr.Error(), "reload session") {
		t.Fatal("reload failure was swallowed")
	}
	r.Handle(context.Background(), "second task")
	if r.LastErr == nil || !strings.Contains(r.LastErr.Error(), "stale history") || len(bodies) != 1 {
		t.Fatalf("stale continuation err=%v calls=%d", r.LastErr, len(bodies))
	}
}
