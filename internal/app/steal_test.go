package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

func TestResumeReportsBusyAndStealNeedsConfirmation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := t.TempDir()
	writeMockProvider(t, root)
	owner, err := session.Open("held", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Append(llm.Message{Role: "user", Content: "kept"}); err != nil {
		t.Fatal(err)
	}

	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, MaxSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	var printed []string
	r := &Runner{App: a, Print: func(format string, args ...any) {
		printed = append(printed, fmt.Sprintf(format, args...))
	}}
	r.Handle(context.Background(), "/resume held")
	joined := strings.Join(printed, "\n")
	if !strings.Contains(joined, "already open for writing") || !strings.Contains(joined, "/steal held") {
		t.Fatalf("busy resume = %q", joined)
	}
	if a.Store.ID == "held" {
		t.Fatal("busy resume switched sessions")
	}

	printed = nil
	r.Handle(context.Background(), "/steal held")
	joined = strings.Join(printed, "\n")
	if !strings.Contains(joined, "explicit confirmation") {
		t.Fatalf("unconfirmed steal = %q", joined)
	}
	if a.Store.ID == "held" {
		t.Fatal("unconfirmed steal switched sessions")
	}

	printed = nil
	confirmed := false
	r.ConfirmSteal = func(ctx context.Context, id string) (bool, error) {
		confirmed = id == "held"
		return false, nil
	}
	r.Handle(context.Background(), "/steal held")
	if !confirmed || !strings.Contains(strings.Join(printed, "\n"), "was not confirmed") {
		t.Fatalf("declined steal confirmed=%v out=%v", confirmed, printed)
	}

	if _, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Resume: "held"}); err == nil || !errors.Is(err, session.ErrSessionBusy) {
		t.Fatalf("ordinary --resume of a busy session = %v, want ErrSessionBusy", err)
	}
	if _, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Steal: true}); err == nil || !strings.Contains(err.Error(), "--steal requires --resume") {
		t.Fatalf("steal without resume = %v", err)
	}
}
