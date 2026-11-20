package app

import (
	"context"
	"minimalharness/internal/agent"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionQuotaRefreshesAfterOwnershipHandoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := session.Open("owner", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	e := &tools.Executor{Root: root, OutputDir: s.OutputDir(), OutputMaxBytes: 10}
	e.SwitchSession(s.ID)
	a := &App{Root: root, Store: s, Cfg: &agent.Config{Exec: e}}
	defer a.Close()
	ctx := context.Background()
	if _, err := e.Bash(ctx, tools.BashArgs{Command: "printf 1234"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	other, err := session.Open("owner", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	second := &tools.Executor{Root: root, OutputDir: other.OutputDir(), OutputMaxBytes: 10}
	if _, err := second.Bash(ctx, tools.BashArgs{Command: "printf abcde"}); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if _, err := a.LoadSession("owner"); err != nil {
		t.Fatal(err)
	}
	out, err := e.Bash(ctx, tools.BashArgs{Command: "printf 123456"})
	t.Logf("third call: %s, %v", out, err)
	entries, err := os.ReadDir(e.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		st, err := os.Stat(filepath.Join(e.OutputDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	if total > 10 {
		t.Fatalf("session quota exceeded after valid ownership handoffs: %d > 10", total)
	}
}
