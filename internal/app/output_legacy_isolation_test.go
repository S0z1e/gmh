package app

import (
	"context"
	"encoding/json"
	"minimalharness/internal/agent"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func legacyFixtureOutputDir(t *testing.T, s *session.Store) string {
	t.Helper()
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &header); err != nil {
		t.Fatal(err)
	}
	id := s.ID
	if value, ok := header["output_id"].(string); ok && value != "" {
		id = value
	}
	return filepath.Join(session.Dir(), id, "bash-output")
}
func TestLegacyAdoptionCannotReuseAnotherOutputOwner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := session.Open("free-name", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	var header map[string]any
	if err := json.Unmarshal(data, &header); err != nil {
		t.Fatal(err)
	}
	delete(header, "output_id") // fixture of a pre-change bound session, with no output yet
	data, err = json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = session.Open("free-name", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	e := &tools.Executor{Root: root, OutputDir: legacyFixtureOutputDir(t, s)}
	e.SwitchSession(s.ID)
	a := &App{Root: root, Store: s, Cfg: &agent.Config{Exec: e}}
	defer a.Close()
	if err := a.RenameSession(s.ID, "renamed-owner"); err != nil {
		t.Fatal(err)
	}
	job, err := e.BashBackground(context.Background(), tools.BashArgs{Command: "printf private-marker"})
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	job.MarkDelivered()
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.Dir(), "unbound.jsonl"), []byte("{\"role\":\"user\",\"content\":\"unrelated history\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.RenameSession("unbound", "free-name"); err != nil {
		t.Fatal(err)
	}
	adopted, err := session.Open("free-name", session.OpenOptions{Workspace: root, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	defer adopted.Close()
	foreign := &tools.Executor{Root: root, OutputDir: legacyFixtureOutputDir(t, adopted)}
	got, err := foreign.Read(tools.ReadArgs{Path: job.OutputFile})
	if err == nil && strings.Contains(got, "private-marker") {
		t.Fatalf("unrelated legacy session read renamed owner's private output: %s", got)
	}
}
