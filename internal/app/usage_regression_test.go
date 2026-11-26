package app

import (
	"context"
	"minimalharness/internal/agent"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageRegressionLedgerFailureDoesNotBlockQuit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := session.Open("native", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r := &Runner{App: &App{Root: root, Store: store, Cfg: &agent.Config{Client: &llm.Client{}, Session: store, Exec: &tools.Executor{Root: root}}}, Print: func(string, ...any) {}}
	if err := r.RefreshUsage(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(filepath.Dir(store.OutputDir()), "usage.jsonl")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUsage(session.UsageAttempt{ID: "attempt", Request: llm.RequestTrace{ID: "attempt"}}); err == nil {
		t.Fatal("expected accounting error")
	}
	if !r.Handle(context.Background(), "/quit") {
		t.Fatalf("quit blocked by accounting error: %v", r.LastErr)
	}
}

func TestUsageRegressionLedgerFailureAllowsResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	healthy, err := session.Open("healthy", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	healthy.Close()
	store, err := session.Open("broken", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r := &Runner{App: &App{Root: root, Store: store, Cfg: &agent.Config{Client: &llm.Client{}, Session: store, Exec: &tools.Executor{Root: root}}}, Print: func(string, ...any) {}}
	if err := r.RefreshUsage(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(filepath.Dir(store.OutputDir()), "usage.jsonl")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUsage(session.UsageAttempt{ID: "attempt", Request: llm.RequestTrace{ID: "attempt"}}); err == nil {
		t.Fatal("expected accounting error")
	}
	r.Handle(context.Background(), "provider turn")
	if r.LastErr == nil || !strings.Contains(r.LastErr.Error(), "load usage") {
		t.Fatalf("bad ledger did not stop provider turn: %v", r.LastErr)
	}
	r.Handle(context.Background(), "/resume healthy")
	if r.App.Store.ID != "healthy" || r.LastErr != nil {
		t.Fatalf("healthy resume blocked: %s %v", r.App.Store.ID, r.LastErr)
	}
	r.App.Close()
}

func TestUsageRecoveryUnlatchesTheNextTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := session.Open("recover", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r := &Runner{App: &App{Root: root, Store: store, Cfg: &agent.Config{Client: &llm.Client{}, Session: store, Exec: &tools.Executor{Root: root}}}, Print: func(string, ...any) {}}
	if err := r.RefreshUsage(); err != nil {
		t.Fatal(err)
	}
	session.SetUsageAppendFault(func() error { return os.ErrPermission })
	defer session.SetUsageAppendFault(nil)
	if err := store.RecordUsage(session.UsageAttempt{ID: "SECONDID", Request: llm.RequestTrace{ID: "SECONDID"}}); err == nil {
		t.Fatal("expected append failure")
	}
	if err := r.RefreshUsage(); err == nil {
		t.Fatal("failed turn published complete accounting")
	}
	session.SetUsageAppendFault(nil)
	if err := r.RefreshUsage(); err != nil {
		t.Fatalf("next turn stayed latched after the journal reopened: %v", err)
	}
}
