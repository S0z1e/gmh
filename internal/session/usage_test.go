package session

import (
	"math"
	"minimalharness/internal/llm"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageLedgerSurvivesRewriteRenameResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("usage", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.25
	for i, id := range []string{"failed", "success", "unknown"} {
		tr := llm.RequestTrace{ID: id, StartedAt: time.Now(), Model: "model", API: "chat", Outcome: "error"}
		if i < 2 {
			tr.Usage = &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
			tr.TokensComplete = i == 1
		}
		a := UsageAttempt{ID: id, Operation: "LLM call", Attempt: i + 1, Request: tr}
		if i == 1 {
			a.EstimateUSD = &cost
			a.RateSource = "old rates"
		}
		if err := s.RecordUsage(a); err != nil {
			t.Fatal(err)
		}
	}
	check := func(s *Store) {
		t.Helper()
		got, err := s.UsageTotals()
		if err != nil {
			t.Fatal(err)
		}
		if got.Attempts != 3 || got.UnreportedAttempts != 1 || got.PartialAttempts != 1 || got.Usage.TotalTokens != 30 || got.EstimateUSD != cost || got.LegacyHistory {
			t.Fatalf("totals=%+v", got)
		}
	}
	check(s)
	if err := s.Rewrite([]llm.Message{{Role: "user", Content: "summary"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("renamed"); err != nil {
		t.Fatal(err)
	}
	check(s)
	s.Close()
	s, err = Open("renamed", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(s)
}

func TestUsageLedgerRecoversTornTailAndFlagsLegacyHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("usage", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	tr := llm.RequestTrace{ID: "one", Usage: &llm.Usage{TotalTokens: 3}}
	if err := s.RecordUsage(UsageAttempt{ID: tr.ID, Request: tr}); err != nil {
		t.Fatal(err)
	}
	path := s.usagePathLocked()
	s.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"version":1,"id":`)
	f.Close()
	s, err = Open("usage", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr.ID = "two"
	if err := s.RecordUsage(UsageAttempt{ID: tr.ID, Request: tr}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UsageTotals()
	if err != nil || got.Attempts != 2 || got.Usage.TotalTokens != 6 {
		t.Fatalf("totals=%+v err=%v", got, err)
	}
	// A damaged complete record is never silently skipped.
	s.Close()
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("broken\n")
	f.Close()
	if s, err := Open("usage", OpenOptions{Workspace: root}); err == nil {
		s.Close()
		t.Fatal("accepted corrupt accounting")
	}
	legacy, err := New("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Append(llm.Message{Role: "user", Content: "past history"}); err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	legacy, err = Open("legacy", OpenOptions{Workspace: root, Rebind: true})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	got, err = legacy.UsageTotals()
	if err != nil || !got.LegacyHistory {
		t.Fatalf("legacy=%+v %v", got, err)
	}
}

func TestUsageLedgerRejectsAliasedJournal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("usage", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	path := s.usagePathLocked()
	s.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if s, err := Open("usage", OpenOptions{Workspace: root}); err == nil {
		s.Close()
		t.Fatal("nonregular journal accepted")
	}
}

func TestUsageInvalidCountersAndCostCannotCorruptReopen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage llm.Usage
		cost  *float64
	}{
		{name: "negative", usage: llm.Usage{TotalTokens: -1}},
		{name: "overflow", usage: llm.Usage{PromptTokens: math.MaxInt, CompletionTokens: 1}},
		{name: "cost", cost: func() *float64 { v := -1.0; return &v }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			s, err := Open("invalid", OpenOptions{Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			tr := llm.RequestTrace{ID: "one", Usage: &tc.usage}
			if err := s.RecordUsage(UsageAttempt{ID: tr.ID, Request: tr, EstimateUSD: tc.cost}); err == nil {
				t.Fatal("invalid accounting accepted")
			}
			s.Close()
			s, err = Open("invalid", OpenOptions{Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			totals, err := s.UsageTotals()
			if err != nil {
				t.Fatal(err)
			}
			if tc.cost == nil && (totals.Attempts != 1 || totals.InvalidAttempts != 1 || totals.PricedAttempts != 0) {
				t.Fatalf("lost invalid attempt: %+v", totals)
			}
		})
	}
}

func TestUsageAggregateOverflowRecordedAsUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("overflow", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, u := range []llm.Usage{{PromptTokens: math.MaxInt}, {CompletionTokens: 1}} {
		id := []string{"first", "second"}[i]
		err := s.RecordUsage(UsageAttempt{ID: id, Request: llm.RequestTrace{ID: id, Usage: &u}})
		if (i == 0 && err != nil) || (i == 1 && err == nil) {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	s.Close()
	s, err = Open("overflow", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	totals, err := s.UsageTotals()
	if err != nil || !totals.Usage.Valid() || totals.Attempts != 2 || totals.InvalidAttempts != 1 {
		t.Fatalf("totals=%+v %v", totals, err)
	}
}

func TestUsageRecoveryRetainsDurableCoverageGap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	s, err := Open("gap", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	cost := 1.0
	tr := llm.RequestTrace{ID: "one", Usage: &llm.Usage{TotalTokens: 1}}
	if err := s.RecordUsage(UsageAttempt{ID: tr.ID, Request: tr, EstimateUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	path := s.usagePathLocked()
	s.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":1,"id":"torn`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for i := 0; i < 2; i++ {
		s, err = Open("gap", OpenOptions{Workspace: root})
		if err != nil {
			t.Fatal(err)
		}
		totals, err := s.UsageTotals()
		s.Close()
		if err != nil || !totals.RecoveryGap || totals.TokenCountsComplete() {
			t.Fatalf("lost durable gap on reopen %d: %+v %v", i, totals, err)
		}
	}
}
