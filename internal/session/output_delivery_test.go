package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/outputreceipt"
)

func TestOutputDeliveryOnlyAcknowledgesDurableCompletedResults(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := Open("receipts", OpenOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dir := s.OutputDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bash-test.log")
	if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	obs := &llm.ToolObservation{Tool: "bash", OutputFile: path, Jobs: []checkpoint.Job{{ID: "bg", Status: checkpoint.Running, OutputFile: path}}}
	if err := s.Append(llm.Message{Role: "tool", Observation: obs}); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(filepath.Dir(dir), "bash-output-delivered", filepath.Base(path))
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatal("running output acknowledged")
	}
	obs.Jobs[0].Status = checkpoint.Done
	if err := s.Append(llm.Message{Role: "user", Kind: "job", Observation: obs}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatal("undelivered output acknowledged")
	}
	obs.Jobs[0].Delivered = true
	if err := s.Append(llm.Message{Role: "user", Kind: "job", Observation: obs}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(receipt); err != nil {
		t.Fatal(err)
	}
	if outputreceipt.Expired(dir, filepath.Base(path), time.Now()) {
		t.Fatal("new receipt already expired")
	}
	if err := s.MarkCompact("summary"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(receipt); err != nil {
		t.Fatal("compaction lost delivery receipt")
	}
	other := filepath.Join(dir, "bash-unsaved.log")
	if err := os.WriteFile(other, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(llm.Message{Role: "tool", Observation: &llm.ToolObservation{Tool: "bash", OutputFile: other}}); err == nil {
		t.Fatal("expected failed append")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "bash-output-delivered", filepath.Base(other))); !os.IsNotExist(err) {
		t.Fatal("failed append acknowledged output")
	}
}
