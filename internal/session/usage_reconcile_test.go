package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/llm"
)

func usageAttempt(id string, tokens int) UsageAttempt {
	return UsageAttempt{ID: id, Request: llm.RequestTrace{ID: id, TokensComplete: true, Usage: &llm.Usage{PromptTokens: tokens, TotalTokens: tokens}}}
}

func TestCommittedMarkerClearedOnReopen(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("reconcile", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	id := "COMMITTEDID"
	if err := s.BeginUsageAttempt(id); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(usageAttempt(id, 2)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(s.usagePathLocked()), usagePendingPrefix+id)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open("reconcile", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	totals, err := s.UsageTotals()
	if err != nil || totals.RecoveryGap || !totals.TokenCountsComplete() || totals.Attempts != 1 {
		t.Fatalf("%+v %v", totals, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("committed marker survived reopen: %v", err)
	}
}

func TestUnmatchedMarkerReconcilesWithoutDeletingEvidence(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("reconcile", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	unknown := "UNKNOWNMARKER"
	notSent := "NOTDISPATCHED"
	if err := s.BeginUsageAttempt(unknown); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginUsageAttempt(notSent); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUsageNotDispatched(notSent); err != nil {
		t.Fatal(err)
	}
	report, err := s.ReconcileUsage()
	if err != nil {
		t.Fatal(err)
	}
	if report.States[unknown] != usageStateUnknown || report.States[notSent] != usageStateNotDispatched {
		t.Fatalf("states=%v", report.States)
	}
	marker := filepath.Join(filepath.Dir(s.usagePathLocked()), usagePendingPrefix+unknown)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unknown marker deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.usagePathLocked()), usagePendingPrefix+notSent)); !os.IsNotExist(err) {
		t.Fatalf("not-dispatched marker still present: %v", err)
	}
	again, err := s.ReconcileUsage()
	if err != nil || again.States[unknown] != usageStateUnknown || again.RecoveryGap != report.RecoveryGap {
		t.Fatalf("reconcile was not idempotent: %+v %v", again, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("second reconcile deleted evidence: %v", err)
	}
	s.Close()
}

func TestTornTailBackupQuarantineClearsRecoveryGap(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("torn", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(usageAttempt("ONEATTEMPT", 1)); err != nil {
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
	s, err = Open("torn", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := s.UsageRecovery()
	if err != nil || !recovery.RecoveryGap || len(recovery.Artifacts) != 1 || recovery.Artifacts[0].Kind != "recovery_backup" {
		t.Fatalf("%+v %v", recovery, err)
	}
	report, err := s.RepairUsage(UsageRepairQuarantine, recovery.Artifacts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if report.RecoveryGap || len(report.Artifacts) != 0 {
		t.Fatalf("quarantine left a gap: %+v", report)
	}
	totals, err := s.UsageTotals()
	if err != nil || totals.RecoveryGap || totals.Attempts != 1 {
		t.Fatalf("%+v %v", totals, err)
	}
	quarantined, err := os.ReadDir(filepath.Join(filepath.Dir(path), usageQuarantineDir))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("backup evidence not retained: %v %v", quarantined, err)
	}
	s.Close()
}

func TestMiddleRecordDeletionIsDetected(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("chain", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"FIRSTID", "MIDDLEID", "LASTID"} {
		if err := s.RecordUsage(usageAttempt(id, 1)); err != nil {
			t.Fatal(err)
		}
	}
	path := s.usagePathLocked()
	s.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines=%d", len(lines))
	}
	kept := strings.Join([]string{lines[0], lines[1], lines[3]}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(kept), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err = Open("chain", OpenOptions{Workspace: root}); err == nil {
		s.Close()
		t.Fatal("deleted middle record was accepted")
	}
}

func TestUsageJournalChainSurvivesReopen(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("chain", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(usageAttempt("FIRSTID", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(usageAttempt("SECONDID", 1)); err != nil {
		t.Fatal(err)
	}
	path := s.usagePathLocked()
	s.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records int
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["chain"]; ok {
			records++
		}
	}
	if records != 2 {
		t.Fatalf("chained records=%d", records)
	}
	s, err = Open("chain", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	totals, err := s.UsageTotals()
	if err != nil || totals.Attempts != 2 || totals.RecoveryGap {
		t.Fatalf("%+v %v", totals, err)
	}
}

func TestAppendFailureRecoversWriterWithoutDroppingEvidence(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("recover", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordUsage(usageAttempt("FIRSTID", 3)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	second := "SECONDID"
	s, err = Open("recover", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginUsageAttempt(second); err != nil {
		t.Fatal(err)
	}
	usageAppendFault = func() error { return os.ErrPermission }
	defer func() { usageAppendFault = nil }()
	defer s.Close()
	if err := s.RecordUsage(usageAttempt(second, 1)); err == nil {
		t.Fatal("append succeeded while the write fault was installed")
	}
	usageAppendFault = nil
	if err := s.RecordUsage(usageAttempt(second, 1)); err != nil {
		t.Fatal(err)
	}
	totals, err := s.UsageTotals()
	if err != nil || totals.Attempts != 2 || totals.Usage.TotalTokens != 4 || totals.RecoveryGap {
		recovery, _ := s.UsageRecovery()
		t.Fatalf("%+v %v artifacts=%+v", totals, err, recovery.Artifacts)
	}
	marker := filepath.Join(filepath.Dir(s.usagePathLocked()), usagePendingPrefix+second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("committed marker survived recovery: %v", err)
	}
}

func TestReconcileReportsCommittedState(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("reconcile", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	id := "COMMITTEDID"
	if err := s.RecordUsage(usageAttempt(id, 1)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(s.usagePathLocked()), usagePendingPrefix+id)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open("reconcile", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report, err := s.ReconcileUsage()
	if err != nil {
		t.Fatal(err)
	}
	if report.States[id] != usageStateCommitted {
		t.Fatalf("states=%v", report.States)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("committed marker still present: %v", err)
	}
}

func TestDamagedJournalNamesArtifactAndQuarantinesIdempotently(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("damaged", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(usageAttempt("FIRSTID", 1)); err != nil {
		t.Fatal(err)
	}
	path := s.usagePathLocked()
	s.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("broken\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Open("damaged", OpenOptions{Workspace: root}); err == nil || !errors.Is(err, ErrUsageDamaged) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "usage-repair") {
		t.Fatalf("damage error did not name the journal and repair: %v", err)
	}
	s, err = Open("damaged", OpenOptions{Workspace: root, RepairDamagedUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report, err := s.RepairUsage(UsageRepairQuarantine, path)
	if err != nil {
		t.Fatal(err)
	}
	if report.RecoveryGap {
		t.Fatalf("quarantine left a gap: %+v", report)
	}
	again, err := s.RepairUsage(UsageRepairQuarantine, path)
	if err != nil || again.RecoveryGap {
		t.Fatalf("second quarantine was not idempotent: %+v %v", again, err)
	}
}
