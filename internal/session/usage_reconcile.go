package session

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	usageReconcileDir       = "usage-reconcile"
	usageQuarantineDir      = "usage-quarantine"
	usageStateCommitted     = "committed"
	usageStateNotDispatched = "not_dispatched"
	usageStateUnknown       = "unknown"
	usageRepairQuarantine   = "quarantine"
	usageRepairClearMarker  = "clear_marker"
	UsageRepairQuarantine   = usageRepairQuarantine
)

// UsageArtifact is one durable file that still blocks complete metering.
type UsageArtifact struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// UsageReconciliation is the audited result of matching pending markers to the
// journal. Committed means the attempt is in the ledger. NotDispatched means
// the marker was created and removed before any provider request. Unknown means
// the marker may still represent real usage and must not be deleted.
type UsageReconciliation struct {
	SessionID      string            `json:"session_id"`
	RecordedAt     time.Time         `json:"recorded_at"`
	States         map[string]string `json:"states"`
	Cleared        []string          `json:"cleared,omitempty"`
	Quarantined    []string          `json:"quarantined,omitempty"`
	Artifacts      []UsageArtifact   `json:"artifacts,omitempty"`
	RecoveryGap    bool              `json:"recovery_gap"`
	ChainSequences int64             `json:"chain_sequences"`
}

func (r UsageReconciliation) blockingArtifacts() []UsageArtifact {
	if r.Artifacts == nil {
		return []UsageArtifact{}
	}
	return r.Artifacts
}

func (s *Store) reconcilePathLocked() string {
	return filepath.Join(filepath.Dir(s.usagePathLocked()), usageReconcileDir)
}

func (s *Store) quarantinePathLocked() string {
	return filepath.Join(filepath.Dir(s.usagePathLocked()), usageQuarantineDir)
}

// UsageRecovery identifies every persistent artifact that keeps RecoveryGap set.
// Strict metering uses this list instead of a bare incomplete flag.
func (s *Store) UsageRecovery() (UsageReconciliation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UsageReconciliation{}, os.ErrClosed
	}
	if err := s.loadUsageLocked(); err != nil {
		return UsageReconciliation{}, err
	}
	return s.usageRecovery, nil
}

// ReconcileUsage clears markers whose journal record is already committed and
// records an explicit state for every other marker. It never deletes a marker
// that might represent a request the journal does not contain.
func (s *Store) ReconcileUsage() (UsageReconciliation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UsageReconciliation{}, os.ErrClosed
	}
	if s.usageWriteFailed {
		return UsageReconciliation{}, fmt.Errorf("usage persistence already failed; reopen before reconciliation")
	}
	if err := s.loadUsageLocked(); err != nil {
		return UsageReconciliation{}, err
	}
	return s.reconcileUsageLocked(usageRepairClearMarker)
}

func (s *Store) reconcileUsageLocked(action string) (UsageReconciliation, error) {
	journal, err := s.readUsageJournalLocked()
	if err != nil {
		return UsageReconciliation{}, err
	}
	pending, err := s.pendingUsageIDsLocked()
	if err != nil {
		return UsageReconciliation{}, err
	}
	backups, err := recoveryBackups(s.usagePathLocked())
	if os.IsNotExist(err) {
		backups, err = nil, nil
	}
	if err != nil {
		return UsageReconciliation{}, err
	}
	states := map[string]string{}
	var cleared []string
	for id := range pending {
		if journal.ids[id] {
			states[id] = usageStateCommitted
			if err := s.clearPendingUsageLocked(id); err != nil {
				return UsageReconciliation{}, err
			}
			cleared = append(cleared, id)
			continue
		}
		noted, err := s.notDispatchedUsageLocked(id)
		if err != nil {
			return UsageReconciliation{}, err
		}
		if noted {
			states[id] = usageStateNotDispatched
			if err := s.clearPendingUsageLocked(id); err != nil {
				return UsageReconciliation{}, err
			}
			cleared = append(cleared, id)
			continue
		}
		states[id] = usageStateUnknown
	}
	for _, id := range s.usageCommitted {
		if _, seen := states[id]; seen {
			continue
		}
		states[id] = usageStateCommitted
		cleared = append(cleared, id)
	}
	notes, err := os.ReadDir(s.reconcilePathLocked())
	if err != nil && !os.IsNotExist(err) {
		return UsageReconciliation{}, err
	}
	for _, note := range notes {
		id := strings.TrimSuffix(note.Name(), ".json")
		if !validBackupID(id) || states[id] != "" {
			continue
		}
		noted, err := s.notDispatchedUsageLocked(id)
		if err != nil {
			return UsageReconciliation{}, err
		}
		if noted {
			states[id] = usageStateNotDispatched
		}
	}
	slices.Sort(cleared)
	report := UsageReconciliation{
		SessionID: s.ID, RecordedAt: time.Now().UTC(), States: states, Cleared: cleared,
		ChainSequences: journal.sequences,
	}
	for id, state := range states {
		if state != usageStateUnknown {
			continue
		}
		report.Artifacts = append(report.Artifacts, UsageArtifact{
			Path:   filepath.Join(filepath.Dir(s.usagePathLocked()), usagePendingPrefix+id),
			Kind:   "pending_marker",
			Reason: "unmatched crash or discontinuity marker; usage may have been spent",
		})
	}
	for _, path := range backups {
		report.Artifacts = append(report.Artifacts, UsageArtifact{
			Path: path, Kind: "recovery_backup", Reason: "torn tail retained as evidence; RecoveryGap stays until quarantine",
		})
	}
	if journal.damaged {
		report.Artifacts = append(report.Artifacts, UsageArtifact{
			Path: s.usagePathLocked(), Kind: "damaged_journal", Reason: journal.damageReason,
		})
	}
	slices.SortFunc(report.Artifacts, func(a, b UsageArtifact) int { return strings.Compare(a.Path, b.Path) })
	report.RecoveryGap = len(report.Artifacts) > 0
	if err := s.auditUsageActionLocked(action, report); err != nil {
		return UsageReconciliation{}, err
	}
	s.usageRecovery = report
	s.usageTotals.RecoveryGap = report.RecoveryGap
	return report, nil
}

type usageJournalView struct {
	ids          map[string]bool
	sequences    int64
	damaged      bool
	damageReason string
}

func (s *Store) readUsageJournalLocked() (usageJournalView, error) {
	view := usageJournalView{ids: map[string]bool{}}
	f, err := s.openUsageLocked(false)
	if os.IsNotExist(err) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 64<<10)
	first := true
	var previous string
	var want int64 = 1
	var chainStarted bool
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		record, chain, _, err := unwrapUsageRecord(line)
		if err != nil {
			view.damaged = true
			view.damageReason = err.Error()
			break
		}
		if err := linkUsageChain(chain, want, previous, chainStarted); err != nil {
			view.damaged = true
			view.damageReason = err.Error()
			break
		}
		var a UsageAttempt
		if err := json.Unmarshal(record, &a); err != nil {
			view.damaged = true
			view.damageReason = "journal line is not valid JSON"
			break
		}
		if a.Version != 1 || (first && a.ID != "") || (!first && (a.ID == "" || a.Request.ID != a.ID)) {
			view.damaged = true
			view.damageReason = "invalid usage journal record"
			break
		}
		if a.Request.Usage != nil && (!a.Request.Usage.Valid()) {
			view.damaged = true
			view.damageReason = "invalid or overflowing usage counters"
			break
		}
		if a.InvalidUsage && (a.Request.Usage != nil || a.EstimateUSD != nil || a.ExactEstimateUSD != "") {
			view.damaged = true
			view.damageReason = "invalid usage marker"
			break
		}
		if _, err := a.exactCost(); err != nil {
			view.damaged = true
			view.damageReason = err.Error()
			break
		}
		if chain != nil {
			chainStarted = true
			previous = hashUsageRecord(append(append([]byte(nil), line...), '\n'))
			want = chain.Sequence + 1
			view.sequences = chain.Sequence
		}
		if !first && a.ID != "" {
			view.ids[a.ID] = true
		}
		first = false
	}
	if err := sc.Err(); err != nil {
		return view, err
	}
	return view, nil
}

// MarkUsageNotDispatched records that a pending marker was removed before any
// provider request. The marker file is left in place until reconciliation so a
// crash cannot turn "about to dispatch" into a silent success.
func (s *Store) MarkUsageNotDispatched(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	if !validBackupID(id) {
		return fmt.Errorf("invalid pending usage identity")
	}
	dir := s.reconcilePathLocked()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return writePrivateJSON(filepath.Join(dir, id+".json"), map[string]string{
		"id": id, "state": usageStateNotDispatched,
	})
}

func (s *Store) notDispatchedUsageLocked(id string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(s.reconcilePathLocked(), id+".json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var note struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &note); err != nil {
		return false, err
	}
	return note.ID == id && note.State == usageStateNotDispatched, nil
}

// RepairUsage quarantines backups and damaged journals, or a named unknown
// marker, without deleting the bytes. Quarantine is the only path that clears
// RecoveryGap for evidence that is not already in the journal.
func (s *Store) RepairUsage(action, artifact string) (UsageReconciliation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UsageReconciliation{}, os.ErrClosed
	}
	if err := s.loadUsageLocked(); err != nil && !errors.Is(err, errUsageDamaged) {
		return UsageReconciliation{}, err
	}
	switch action {
	case usageRepairQuarantine:
		if err := s.quarantineUsageLocked(artifact); err != nil {
			return UsageReconciliation{}, err
		}
		s.usageLoaded = false
		s.usageInfo = nil
		if err := s.loadUsageLocked(); err != nil && !errors.Is(err, errUsageDamaged) {
			return UsageReconciliation{}, err
		}
		return s.reconcileUsageLocked(action)
	default:
		return UsageReconciliation{}, fmt.Errorf("unknown usage repair %q; use %s", action, usageRepairQuarantine)
	}
}

var errUsageDamaged = errors.New("usage journal is damaged")

// ErrUsageDamaged is a journal whose records cannot be trusted. The error
// text also names the file and the quarantine command.
var ErrUsageDamaged = errUsageDamaged

func (s *Store) damagedJournal(reason string) error {
	return errors.Join(ErrUsageDamaged, BlockingUsageError([]UsageArtifact{{
		Path: s.usagePathLocked(), Kind: "damaged_journal", Reason: reason,
	}}))
}

func (s *Store) quarantineUsageLocked(artifact string) error {
	if artifact == "" {
		return fmt.Errorf("quarantine needs the artifact path named by the strict error")
	}
	clean := filepath.Clean(artifact)
	family := filepath.Dir(s.usagePathLocked())
	rel, err := filepath.Rel(family, clean)
	if err != nil || rel == "." || rel == ".." || strings.Contains(rel, string(os.PathSeparator)) {
		return fmt.Errorf("artifact %s is outside this session usage family", artifact)
	}
	base := filepath.Base(clean)
	allowed := base == "usage.jsonl" || strings.HasPrefix(base, usagePendingPrefix) || strings.Contains(base, ".jsonl-torn-")
	if !allowed || strings.Contains(base, "..") {
		return fmt.Errorf("artifact %s is not a usage journal, marker, or recovery backup", artifact)
	}
	info, err := os.Lstat(clean)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact %s is not a regular file", artifact)
	}
	destDir := s.quarantinePathLocked()
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return err
	}
	dest := filepath.Join(destDir, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+rand.Text()+"-"+base)
	if err := os.Rename(clean, dest); err != nil {
		return err
	}
	if err := syncRecoveryDirectory(destDir); err != nil {
		return err
	}
	return syncRecoveryDirectory(family)
}

func (s *Store) auditUsageActionLocked(action string, report UsageReconciliation) error {
	return s.auditLocked(struct {
		Kind   string              `json:"kind"`
		Action string              `json:"action"`
		Report UsageReconciliation `json:"report"`
	}{"usage_reconciliation", action, report})
}

func writePrivateJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp := path + ".tmp-" + rand.Text()
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		os.Remove(temp)
		return errors.Join(writeErr, syncErr, closeErr)
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(path))
}

func formatUsageArtifacts(artifacts []UsageArtifact) string {
	if len(artifacts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		parts = append(parts, fmt.Sprintf("%s (%s: %s)", a.Path, a.Kind, a.Reason))
	}
	return strings.Join(parts, "; ")
}

// blockingUsageError is the strict-mode failure. It names each artifact and the
// repair that clears it, so a repeated cap failure is actionable.
func BlockingUsageError(artifacts []UsageArtifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	return fmt.Errorf("strict metering blocked by %s; repair: harness usage-repair --root <project> --session <id> --quarantine <artifact>", formatUsageArtifacts(artifacts))
}
