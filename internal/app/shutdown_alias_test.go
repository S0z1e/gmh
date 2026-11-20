package app

import (
	"context"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedAliasRenamePreservesJobOwnership(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.RenameSession(a.Store.ID, "mixed-owner"); err != nil {
		t.Fatal(err)
	}
	job, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "exec sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer job.Cancel()
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(a.Store.Path), "MiXeD-OWNER.jsonl")); err != nil {
		t.Skip("case sensitive filesystem")
	}
	if err := a.RenameSession("MiXeD-OWNER", "renamed-owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoadSession("renamed-owner"); err != nil {
		t.Fatal(err)
	}
	if jobs := a.Cfg.Exec.Jobs(); len(jobs) != 1 || jobs[0] != job {
		t.Fatal("alias rename lost retained job registry")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := session.Load("renamed-owner")
	if err != nil || len(saved) != 1 || !job.Delivered() {
		t.Fatalf("lost result after alias rename: %v %+v", err, saved)
	}
}
