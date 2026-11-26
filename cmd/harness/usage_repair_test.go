package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

func TestUsageRepairQuarantinesDamagedJournalTwice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	store, err := session.Open("repair", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	id := "FIRSTID"
	if err := store.RecordUsage(session.UsageAttempt{ID: id, Request: llm.RequestTrace{ID: id, TokensComplete: true, Usage: &llm.Usage{TotalTokens: 1}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session.Dir(), store.OutputID(), "usage.jsonl")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("broken\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	args := []string{"--session", "repair", "--root", root, "--quarantine", path}
	var stdout, stderr bytes.Buffer
	if err := usageRepair(args, &stdout, &stderr); err != nil {
		t.Fatalf("repair: %v\n%s", err, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := usageRepair(args, &stdout, &stderr); err != nil {
		t.Fatalf("second repair: %v\n%s", err, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("recovery gap cleared")) {
		t.Fatalf("stdout=%s", stdout.String())
	}
}
