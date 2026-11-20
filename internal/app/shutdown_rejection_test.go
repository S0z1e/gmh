package app

import (
	"context"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"os"
	"testing"
)

func TestRejectedBashAuditRetainsLease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	store := a.Store
	entered, release := make(chan struct{}), make(chan struct{})
	a.Cfg.Exec.Audit = func(e tools.AuditEntry) { close(entered); <-release; _ = store.Audit(e) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Cfg.Exec.Bash(context.Background(), tools.BashArgs{Command: "rm -rf /"})
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closeErr := a.CloseContext(ctx)
	reopened, openErr := session.Open(store.ID, session.OpenOptions{Workspace: root})
	if reopened != nil {
		defer reopened.Close()
	}
	close(release)
	<-done
	if _, err := os.Stat(store.AuditPath()); err != nil {
		t.Fatal(err)
	}
	if closeErr == nil || openErr == nil {
		t.Fatalf("shutdown released session while rejected audit pending: close=%v open=%v", closeErr, openErr)
	}
}
