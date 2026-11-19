package session

import (
	"crypto/rand"
	"minimalharness/internal/llm"
	"os"
	"path/filepath"
	"testing"
)

func TestUsagePendingSurvivesColdReopen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("pending", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	id := rand.Text()
	if err := s.BeginUsageAttempt(id); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open("pending", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	totals, err := s.UsageTotals()
	if err != nil || !totals.RecoveryGap || totals.TokenCountsComplete() {
		t.Fatalf("%+v %v", totals, err)
	}
}
func TestUsagePendingCompletionReconcilesCommittedRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("pending", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	id := rand.Text()
	if err := s.BeginUsageAttempt(id); err != nil {
		t.Fatal(err)
	}
	a := UsageAttempt{ID: id, Request: llm.RequestTrace{ID: id, TokensComplete: true, Usage: &llm.Usage{PromptTokens: 1, TotalTokens: 1}}}
	if err := s.RecordUsage(a); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(s.usagePathLocked()), usagePendingPrefix+id)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("completed marker still exists: %v", err)
	}
	// Simulate the crash window after journal Sync and before marker removal.
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open("pending", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	totals, err := s.UsageTotals()
	if err != nil || totals.RecoveryGap || !totals.TokenCountsComplete() || totals.Attempts != 1 {
		t.Fatalf("%+v %v", totals, err)
	}
}
