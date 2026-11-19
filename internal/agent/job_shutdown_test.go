package agent

import (
	"context"
	"testing"

	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestSaveJobResultsRetriesWithoutLosingOrDuplicatingOutcome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := session.Open("shutdown", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	e := &tools.Executor{Root: root}
	job, err := e.BashBackground(context.Background(), tools.BashArgs{Command: "printf failed-output; exit 2"})
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := SaveJobResults(store, []*tools.Job{job}); err == nil || job.Delivered() {
		t.Fatalf("failed append acknowledged result: err=%v delivered=%t", err, job.Delivered())
	}
	store, err = session.Open("shutdown", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 2; i++ {
		if err := SaveJobResults(store, []*tools.Job{job}); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Kind != "job" || !saved[0].ToolError || saved[0].Observation == nil || len(saved[0].Observation.Jobs) != 1 || *saved[0].Observation.Jobs[0].ExitCode != 2 {
		t.Fatalf("lost or repeated failed result: %+v", saved)
	}
}
