package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStopCapReportsUnfinished(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("cap")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hooks := &fakePluginRunner{stopBlocks: 99, stopReason: "required work still missing"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textSSE("done"))
	}))
	defer srv.Close()
	cfg := &Config{Session: store, Client: &llm.Client{APIKey: "test", Model: "test", API: "chat", BaseURL: srv.URL}, Exec: &tools.Executor{Root: t.TempDir(), Plugins: hooks}, Tools: ToolDefs(), MaxSteps: 20}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "task"}})
	if err == nil || string(cfg.LastTerminalReason) != "stop_cap" {
		t.Fatalf("stop veto cap incorrectly allowed completion: text=%q reason=%q err=%v", text, cfg.LastTerminalReason, err)
	}
	history, loadErr := session.Load(store.ID)
	if loadErr != nil || len(history) == 0 || history[len(history)-1].Kind != "task_outcome" {
		t.Fatalf("incomplete outcome missing from resumed context: %v %v", history, loadErr)
	}
}

func TestTaskOutcomeDoesNotLeakIntoNextTurn(t *testing.T) {
	hooks := &fakePluginRunner{stopBlocks: 99, stopReason: "missing"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textSSE("answer"))
	}))
	defer srv.Close()
	cfg := &Config{Client: &llm.Client{APIKey: "test", Model: "test", API: "chat", BaseURL: srv.URL}, Exec: &tools.Executor{Root: t.TempDir(), Plugins: hooks}, Tools: ToolDefs(), MaxSteps: 20}
	_, _, err := cfg.Run(context.Background(), nil)
	if err == nil || cfg.LastTaskOutcome != OutcomeIncomplete {
		t.Fatalf("first outcome=%s err=%v", cfg.LastTaskOutcome, err)
	}
	hooks.stopBlocks = 0
	var last Event
	cfg.OnEvent = func(ev Event) {
		if ev.Kind == EventDone || ev.Kind == EventError {
			last = ev
		}
	}
	_, _, err = cfg.Run(context.Background(), nil)
	if err != nil || cfg.LastTaskOutcome != "" || last.TaskOutcome != "" || last.TerminalReason != TerminalEndTurn {
		t.Fatalf("end_turn asserted/stale outcome: event=%+v err=%v", last, err)
	}
}

func TestStopHookCanPassOnFinalFeedbackRound(t *testing.T) {
	hooks := &fakePluginRunner{stopBlocks: MaxStopBlocks, stopReason: "fix this"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textSSE("answer"))
	}))
	defer srv.Close()
	cfg := &Config{Client: &llm.Client{APIKey: "test", Model: "test", API: "chat", BaseURL: srv.URL}, Exec: &tools.Executor{Root: t.TempDir(), Plugins: hooks}, Tools: ToolDefs(), MaxSteps: 20}
	_, _, err := cfg.Run(context.Background(), nil)
	if err != nil || cfg.LastTerminalReason != TerminalEndTurn {
		t.Fatalf("resolved veto still capped: %v", err)
	}
	if hooks.stopCalls != MaxStopBlocks+1 {
		t.Fatalf("final answer not checked: calls=%d", hooks.stopCalls)
	}
}
