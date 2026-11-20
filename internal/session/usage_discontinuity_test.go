package session

import (
	"crypto/rand"
	"minimalharness/internal/llm"
	"os"
	"testing"
)

func TestUsageJournalDiscontinuityCannotRebaseCachedTotals(t *testing.T) {
	for _, change := range []string{"delete", "truncate", "replace"} {
		t.Run(change, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			root := t.TempDir()
			s, err := Open("changed", OpenOptions{Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			a := UsageAttempt{ID: rand.Text(), Request: llm.RequestTrace{Usage: &llm.Usage{PromptTokens: 1, TotalTokens: 1}, TokensComplete: true}}
			a.Request.ID = a.ID
			if err := s.RecordUsage(a); err != nil {
				t.Fatal(err)
			}
			path := s.usagePathLocked()
			switch change {
			case "delete":
				err = os.Remove(path)
			case "truncate":
				err = os.Truncate(path, 0)
			case "replace":
				var b []byte
				b, err = os.ReadFile(path)
				if err == nil {
					err = os.Remove(path)
				}
				if err == nil {
					err = os.WriteFile(path, b, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			a.ID = rand.Text()
			a.Request.ID = a.ID
			if err := s.RecordUsage(a); err == nil {
				t.Fatal("journal discontinuity silently accepted")
			}
			s.Close()
			s, err = Open("changed", OpenOptions{Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			totals, err := s.UsageTotals()
			if err != nil || !totals.RecoveryGap || totals.TokenCountsComplete() {
				t.Fatalf("cold reopen lost discontinuity: %+v %v", totals, err)
			}
		})
	}
}
