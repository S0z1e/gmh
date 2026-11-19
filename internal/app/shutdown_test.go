package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestQuitStopsAndSavesAllOwningSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Store.Close() }()
	firstID := a.Store.ID
	first, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "printf old-marker; exec sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cancel()
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	secondID := a.Store.ID
	second, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "printf new-marker; exec sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cancel()
	r := &Runner{App: a, Print: func(string, ...any) {}}
	if !r.Handle(context.Background(), "/quit") {
		t.Fatal("quit refused")
	}
	for i, job := range []*tools.Job{first, second} {
		select {
		case <-job.Done():
		case <-time.After(time.Second):
			t.Fatalf("job %s survived quit", job.ID)
		}
		owner := []string{firstID, secondID}[i]
		history, err := session.Load(owner)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, m := range history {
			if m.Kind == "job" && m.Observation != nil {
				for _, j := range m.Observation.Jobs {
					if j.ID == job.ID && j.Started.Equal(job.Started) {
						found++
					}
				}
			}
		}
		if found != 1 {
			t.Fatalf("owner %s has %d results for %s, history=%+v", owner, found, job.ID, history)
		}
		for _, m := range history {
			if strings.Contains(m.Content, []string{"new-marker", "old-marker"}[i]) {
				t.Fatalf("foreign outcome in owner %s: %s", owner, fmt.Sprint(m))
			}
		}
	}
}

func TestRetainedSessionKeepsWriterAndCanResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test")
	root := t.TempDir()
	writeMockProvider(t, root)
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	owner := a.Store
	job, err := a.Cfg.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "exec sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if competing, err := session.Open(owner.ID, session.OpenOptions{Workspace: root}); err == nil {
		competing.Close()
		t.Fatal("retained session released its writer while owning undelivered work")
	}
	if err := a.RenameSession(owner.ID, "retained-renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoadSession("retained-renamed"); err != nil || a.Store != owner {
		t.Fatalf("retained writer was not reused: %v", err)
	}
	if len(a.Cfg.Exec.Jobs()) != 1 || a.Cfg.Exec.Jobs()[0] != job {
		t.Fatal("renamed retained session lost its job")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := session.Open("retained-renamed", session.OpenOptions{Workspace: root}); err != nil {
		t.Fatalf("shutdown retained the writer after saving its results: %v", err)
	} else {
		reopened.Close()
	}
}
