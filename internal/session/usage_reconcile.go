package session

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	usageReconcileDir       = "usage-reconcile"
	usageQuarantineDir      = "usage-quarantine"
	usageQuarantinePlanFile = "usage-quarantine-pending.json"
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

// UsageRepairOptions configures RepairUsageWithOptions.
type UsageRepairOptions struct {
	// ForceHealthy overrides the refusal to quarantine a usage journal that
	// loads cleanly. Overrides are audited.
	ForceHealthy bool
}

// UsageReconciliation is the audited result of matching pending markers to the
// journal. Committed means the attempt is in the ledger. NotDispatched means
// the marker was created and removed before any provider request. Unknown means
// the marker may still represent real usage and must not be deleted.
type UsageReconciliation struct {
	SessionID          string             `json:"session_id"`
	RecordedAt         time.Time          `json:"recorded_at"`
	States             map[string]string  `json:"states"`
	Cleared            []string           `json:"cleared,omitempty"`
	Quarantined        []string           `json:"quarantined,omitempty"`
	Artifacts          []UsageArtifact    `json:"artifacts,omitempty"`
	RecoveryGap        bool               `json:"recovery_gap"`
	ChainSequences     int64              `json:"chain_sequences"`
	CarryForward       *UsageCarryForward `json:"carry_forward,omitempty"`
	HealthyOverride    bool               `json:"healthy_override,omitempty"`
	LowerBoundAccepted bool               `json:"lower_bound_accepted,omitempty"`
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
		if err := s.reopenUsageAfterWriteFailureLocked(); err != nil {
			return UsageReconciliation{}, err
		}
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
		ChainSequences: journal.sequences, CarryForward: journal.carryForward,
		LowerBoundAccepted: journal.totals.LowerBoundBaselineReady(),
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
	if action != usageRepairQuarantine {
		if err := s.auditUsageActionLocked(action, report); err != nil {
			return UsageReconciliation{}, err
		}
	}
	s.usageRecovery = report
	s.usageTotals.RecoveryGap = report.RecoveryGap
	return report, nil
}

type usageJournalScan struct {
	ids          map[string]bool
	totals       UsageTotals
	carryForward *UsageCarryForward
	sequences    int64
	chainStarted bool
	lastDigest   string
	hasHeader    bool
	hasNonHeader bool
	damaged      bool
	damageReason string
}

func scanUsageJournal(r io.Reader, onValid func(UsageAttempt) error) (usageJournalScan, error) {
	scan := usageJournalScan{ids: map[string]bool{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64<<10)
	first := true
	var previous string
	var want int64 = 1
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		record, chain, _, err := unwrapUsageRecord(line)
		if err != nil {
			scan.damaged = true
			scan.damageReason = err.Error()
			break
		}
		if err := linkUsageChain(chain, want, previous, scan.chainStarted); err != nil {
			scan.damaged = true
			scan.damageReason = err.Error()
			break
		}
		var a UsageAttempt
		if err := json.Unmarshal(record, &a); err != nil {
			scan.damaged = true
			scan.damageReason = "journal line is not valid JSON"
			break
		}
		if a.Version != 1 || (first && a.ID != "") || (!first && (a.ID == "" || a.Request.ID != a.ID)) {
			scan.damaged = true
			scan.damageReason = "invalid usage journal record"
			break
		}
		if first && (a.AcceptLowerBound || a.InvalidUsage || a.Request.Usage != nil || a.EstimateUSD != nil || a.ExactEstimateUSD != "") {
			scan.damaged = true
			scan.damageReason = "invalid usage journal header"
			break
		}
		if !first && (a.CarryForward != nil || a.LegacyHistory) {
			scan.damaged = true
			scan.damageReason = "invalid usage journal record"
			break
		}
		if !first && a.AcceptLowerBound && (a.Request.Usage != nil || a.EstimateUSD != nil || a.ExactEstimateUSD != "" || a.InvalidUsage) {
			scan.damaged = true
			scan.damageReason = "invalid usage acknowledgment record"
			break
		}
		if a.Request.Usage != nil && (!a.Request.Usage.Valid() || !scan.totals.Usage.CanAdd(*a.Request.Usage)) {
			scan.damaged = true
			scan.damageReason = "invalid or overflowing usage counters"
			break
		}
		if a.InvalidUsage && (a.Request.Usage != nil || a.EstimateUSD != nil || a.ExactEstimateUSD != "") {
			scan.damaged = true
			scan.damageReason = "invalid usage marker"
			break
		}
		nextTotals := scan.totals
		if err := nextTotals.add(a); err != nil {
			scan.damaged = true
			scan.damageReason = err.Error()
			break
		}
		scan.totals = nextTotals
		if chain != nil {
			scan.chainStarted = true
			previous = hashUsageRecord(append(append([]byte(nil), line...), '\n'))
			scan.lastDigest = previous
			want = chain.Sequence + 1
			scan.sequences = chain.Sequence
		}
		if first {
			scan.hasHeader = true
			scan.carryForward = a.CarryForward
		} else {
			scan.hasNonHeader = true
			if !a.AcceptLowerBound && a.ID != "" {
				scan.ids[a.ID] = true
			}
		}
		if onValid != nil {
			if err := onValid(a); err != nil {
				return scan, err
			}
		}
		first = false
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			scan.damaged = true
			scan.damageReason = "usage journal record exceeds 64 KiB limit"
			return scan, nil
		}
		return scan, err
	}
	return scan, nil
}

func (s *Store) readUsageJournalLocked() (usageJournalScan, error) {
	f, err := s.openUsageLocked(false)
	if os.IsNotExist(err) {
		return usageJournalScan{ids: map[string]bool{}}, nil
	}
	if err != nil {
		return usageJournalScan{}, err
	}
	defer f.Close()
	return scanUsageJournal(f, nil)
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
	return s.RepairUsageWithOptions(action, artifact, UsageRepairOptions{})
}

// RepairUsageWithOptions behaves like RepairUsage with explicit repair options
// such as ForceHealthy to quarantine a journal that loads cleanly.
func (s *Store) RepairUsageWithOptions(action, artifact string, opts UsageRepairOptions) (UsageReconciliation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UsageReconciliation{}, os.ErrClosed
	}
	switch action {
	case usageRepairQuarantine:
		if s.recoveredQuarantine != nil && len(s.recoveredQuarantine.Quarantined) > 0 && filepath.Clean(s.recoveredQuarantine.Quarantined[0]) == filepath.Clean(artifact) {
			report := *s.recoveredQuarantine
			s.recoveredQuarantine = nil
			return report, nil
		}
		pendingReport, recovered, err := s.recoverPendingQuarantineLocked()
		if err != nil {
			return UsageReconciliation{}, err
		}
		if recovered && len(pendingReport.Quarantined) > 0 && filepath.Clean(pendingReport.Quarantined[0]) == filepath.Clean(artifact) {
			return pendingReport, nil
		}
		if err := s.loadUsageAfterRecoveryLocked(); err != nil && !errors.Is(err, errUsageDamaged) {
			return UsageReconciliation{}, err
		}
		plan, planPath, staged, err := s.stageQuarantineLocked(artifact, opts)
		if err != nil {
			return UsageReconciliation{}, err
		}
		if !staged {
			report, err := s.reconcileUsageLocked(action)
			if err != nil {
				return UsageReconciliation{}, err
			}
			s.usageRecovery = report
			if err := s.auditUsageActionLocked(action, report); err != nil {
				return UsageReconciliation{}, err
			}
			return report, nil
		}
		if err := s.executeQuarantineMovesLocked(plan); err != nil {
			return UsageReconciliation{}, err
		}
		report, _, err := s.finishQuarantineLocked(plan, planPath, false)
		return report, err
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

type pendingUsageQuarantine struct {
	Version         int                `json:"version"`
	Artifact        string             `json:"artifact"`
	Source          string             `json:"source"`
	Dest            string             `json:"dest"`
	StagedHeader    string             `json:"staged_header,omitempty"`
	LegacyHistory   bool               `json:"legacy_history,omitempty"`
	HealthyOverride bool               `json:"healthy_override,omitempty"`
	CarryForward    *UsageCarryForward `json:"carry_forward,omitempty"`
	CommittedIDs    []string           `json:"committed_ids,omitempty"`
}

func (s *Store) quarantinePlanPathLocked() string {
	return filepath.Join(filepath.Dir(s.usagePathLocked()), usageQuarantinePlanFile)
}

func (s *Store) validateUsageArtifactPathLocked(artifact string) (clean, base string, err error) {
	if artifact == "" {
		return "", "", fmt.Errorf("quarantine needs the artifact path named by the strict error")
	}
	clean = filepath.Clean(artifact)
	family := filepath.Dir(s.usagePathLocked())
	rel, relErr := filepath.Rel(family, clean)
	if relErr != nil || rel == "." || rel == ".." || strings.Contains(rel, string(os.PathSeparator)) {
		return "", "", fmt.Errorf("artifact %s is outside this session usage family", artifact)
	}
	base = filepath.Base(clean)
	allowed := base == "usage.jsonl" || strings.HasPrefix(base, usagePendingPrefix) || strings.Contains(base, ".jsonl-torn-")
	if !allowed || strings.Contains(base, "..") {
		return "", "", fmt.Errorf("artifact %s is not a usage journal, marker, or recovery backup", artifact)
	}
	return clean, base, nil
}

func checkRegularOrMissing(path, label string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %s is not a regular file", label, path)
	}
	return info, nil
}

func (s *Store) validatePendingQuarantineLocked(plan pendingUsageQuarantine) error {
	if plan.Version != 1 || plan.Artifact == "" || plan.Source == "" || plan.Dest == "" {
		return fmt.Errorf("invalid pending quarantine plan")
	}
	cleanSource, base, err := s.validateUsageArtifactPathLocked(plan.Source)
	if err != nil || cleanSource != plan.Source {
		return fmt.Errorf("invalid pending quarantine source path: %s", plan.Source)
	}
	cleanArtifact, _, err := s.validateUsageArtifactPathLocked(plan.Artifact)
	if err != nil || cleanArtifact != cleanSource {
		return fmt.Errorf("invalid pending quarantine artifact path: %s", plan.Artifact)
	}
	destDir := s.quarantinePathLocked()
	cleanDest := filepath.Clean(plan.Dest)
	relDest, relErr := filepath.Rel(destDir, cleanDest)
	if cleanDest != plan.Dest || relErr != nil || relDest == "." || relDest == ".." || strings.Contains(relDest, string(os.PathSeparator)) || !strings.HasSuffix(filepath.Base(cleanDest), "-"+base) {
		return fmt.Errorf("invalid pending quarantine destination path: %s", plan.Dest)
	}
	if _, err := checkRegularOrMissing(plan.Source, "quarantine source"); err != nil {
		return err
	}
	if _, err := checkRegularOrMissing(plan.Dest, "quarantine destination"); err != nil {
		return err
	}
	if base == "usage.jsonl" {
		family := filepath.Dir(s.usagePathLocked())
		cleanStaged := filepath.Clean(plan.StagedHeader)
		relStaged, relErr := filepath.Rel(family, cleanStaged)
		stagedBase := filepath.Base(cleanStaged)
		nonce := strings.TrimPrefix(stagedBase, "usage.jsonl.tmp-")
		if plan.StagedHeader == "" || cleanStaged != plan.StagedHeader || relErr != nil || relStaged == "." || relStaged == ".." || strings.Contains(relStaged, string(os.PathSeparator)) || !strings.HasPrefix(stagedBase, "usage.jsonl.tmp-") || !validBackupID(nonce) {
			return fmt.Errorf("invalid pending quarantine staged header path: %s", plan.StagedHeader)
		}
		if _, err := checkRegularOrMissing(plan.StagedHeader, "staged header"); err != nil {
			return err
		}
		if plan.CarryForward == nil {
			return fmt.Errorf("pending journal quarantine plan missing carry-forward data")
		}
		if err := plan.CarryForward.validate(); err != nil {
			return err
		}
		if plan.CarryForward.QuarantinedFile != plan.Dest {
			return fmt.Errorf("pending quarantine carry-forward destination mismatch")
		}
	} else {
		if plan.StagedHeader != "" || plan.CarryForward != nil {
			return fmt.Errorf("unexpected staged header or carry-forward on non-journal quarantine plan")
		}
	}
	return nil
}

func (s *Store) stageQuarantineLocked(artifact string, opts UsageRepairOptions) (pendingUsageQuarantine, string, bool, error) {
	clean, base, err := s.validateUsageArtifactPathLocked(artifact)
	if err != nil {
		return pendingUsageQuarantine{}, "", false, err
	}
	info, err := checkRegularOrMissing(clean, "artifact")
	if err != nil {
		return pendingUsageQuarantine{}, "", false, err
	}
	if info == nil {
		return pendingUsageQuarantine{}, "", false, nil
	}
	family := filepath.Dir(s.usagePathLocked())
	destDir := s.quarantinePathLocked()
	dest := filepath.Join(destDir, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+rand.Text()+"-"+base)
	plan := pendingUsageQuarantine{
		Version:  1,
		Artifact: artifact,
		Source:   clean,
		Dest:     dest,
	}
	if base == "usage.jsonl" {
		view, err := s.readUsageJournalLocked()
		if err != nil {
			return pendingUsageQuarantine{}, "", false, err
		}
		if !view.damaged && !opts.ForceHealthy {
			return pendingUsageQuarantine{}, "", false, fmt.Errorf("%w: %s loads cleanly; pass --allow-healthy to override", ErrUsageHealthy, clean)
		}
		legacyHistory := view.totals.LegacyHistory
		if !view.hasHeader {
			if hasMsgs, msgErr := s.transcriptHasMessagesLocked(); msgErr == nil && hasMsgs {
				legacyHistory = true
			}
		}
		estUSD, exactUSD, err := exactCostFields(view.totals.ExactCost())
		if err != nil {
			return pendingUsageQuarantine{}, "", false, err
		}
		healthyOverride := !view.damaged && opts.ForceHealthy
		cf := &UsageCarryForward{
			Usage:              view.totals.Usage,
			Attempts:           view.totals.Attempts,
			InvalidAttempts:    view.totals.InvalidAttempts,
			UnreportedAttempts: view.totals.UnreportedAttempts,
			PartialAttempts:    view.totals.PartialAttempts,
			PricedAttempts:     view.totals.PricedAttempts,
			RejectedAttempts:   view.totals.RejectedAttempts,
			TransientAttempts:  view.totals.TransientAttempts,
			EstimateUSD:        estUSD,
			ExactEstimateUSD:   exactUSD,
			QuarantinedFile:    dest,
			QuarantinedAt:      time.Now().UTC(),
			LowerBound:         view.damaged || view.totals.LowerBound,
			LowerBoundAccepted: !view.damaged && view.totals.LowerBoundBaselineReady(),
			HealthyOverride:    healthyOverride,
			DamageReason:       view.damageReason,
		}
		header := UsageAttempt{
			Version:       1,
			LegacyHistory: legacyHistory,
			CarryForward:  cf,
		}
		headerBytes, err := json.Marshal(header)
		if err != nil {
			return pendingUsageQuarantine{}, "", false, err
		}
		if len(headerBytes) >= 64<<10 {
			return pendingUsageQuarantine{}, "", false, fmt.Errorf("carry-forward header exceeds 64 KiB journal record limit")
		}
		stagedHeader := clean + ".tmp-" + rand.Text()
		if err := writePrivateJSONNoRename(stagedHeader, header); err != nil {
			return pendingUsageQuarantine{}, "", false, err
		}
		if err := syncRecoveryDirectory(family); err != nil {
			os.Remove(stagedHeader)
			return pendingUsageQuarantine{}, "", false, err
		}
		plan.StagedHeader = stagedHeader
		plan.LegacyHistory = legacyHistory
		plan.HealthyOverride = healthyOverride
		plan.CarryForward = cf
		for id := range view.ids {
			plan.CommittedIDs = append(plan.CommittedIDs, id)
		}
		slices.Sort(plan.CommittedIDs)
	}
	planPath := s.quarantinePlanPathLocked()
	if err := writePrivateJSON(planPath, plan); err != nil {
		if plan.StagedHeader != "" {
			os.Remove(plan.StagedHeader)
		}
		return pendingUsageQuarantine{}, "", false, err
	}
	return plan, planPath, true, nil
}

func (s *Store) executeQuarantineMovesLocked(plan pendingUsageQuarantine) error {
	destDir := filepath.Dir(plan.Dest)
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(plan.Source); err == nil {
		if err := os.Rename(plan.Source, plan.Dest); err != nil {
			return err
		}
		if err := syncRecoveryDirectory(destDir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if plan.StagedHeader != "" {
		if _, err := os.Lstat(plan.StagedHeader); err == nil {
			if err := os.Rename(plan.StagedHeader, plan.Source); err != nil {
				return err
			}
			if err := syncRecoveryDirectory(filepath.Dir(plan.Source)); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	} else {
		if err := syncRecoveryDirectory(filepath.Dir(plan.Source)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) finishQuarantineLocked(plan pendingUsageQuarantine, planPath string, holdingMgmt bool) (UsageReconciliation, bool, error) {
	if len(plan.CommittedIDs) > 0 {
		pending, err := s.pendingUsageIDsLocked()
		if err != nil {
			return UsageReconciliation{}, false, err
		}
		for _, id := range plan.CommittedIDs {
			s.rememberCommitted(id)
			if pending[id] {
				if err := s.clearPendingUsageLocked(id); err != nil {
					return UsageReconciliation{}, false, err
				}
			}
		}
	}
	s.usageLoaded = false
	s.usageInfo = nil
	s.usageTotals = UsageTotals{}
	s.usageChainSeq = 0
	s.usageChainDigest = ""
	s.usageChainOn = false
	if err := s.loadUsageAfterRecoveryLocked(); err != nil && !errors.Is(err, errUsageDamaged) {
		return UsageReconciliation{}, false, err
	}
	report, err := s.reconcileUsageLocked(usageRepairQuarantine)
	if err != nil {
		return UsageReconciliation{}, false, err
	}
	report.Quarantined = append(report.Quarantined, plan.Artifact)
	report.HealthyOverride = plan.HealthyOverride
	if plan.CarryForward != nil {
		report.CarryForward = plan.CarryForward
	}
	s.usageRecovery = report
	if err := s.auditUsageActionWithMgmtLocked(usageRepairQuarantine, report, holdingMgmt); err != nil {
		return UsageReconciliation{}, false, err
	}
	if err := os.Remove(planPath); err != nil && !os.IsNotExist(err) {
		return UsageReconciliation{}, false, err
	}
	if err := syncRecoveryDirectory(filepath.Dir(planPath)); err != nil {
		return UsageReconciliation{}, false, err
	}
	return report, true, nil
}

func (s *Store) recoverPendingQuarantineLocked() (UsageReconciliation, bool, error) {
	return s.recoverPendingQuarantineWithMgmtLocked(false)
}

func (s *Store) recoverPendingQuarantineWithMgmtLocked(holdingMgmt bool) (UsageReconciliation, bool, error) {
	planPath := s.quarantinePlanPathLocked()
	info, err := checkRegularOrMissing(planPath, "pending quarantine plan")
	if err != nil {
		return UsageReconciliation{}, false, err
	}
	if info == nil {
		return UsageReconciliation{}, false, nil
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		return UsageReconciliation{}, false, err
	}
	var plan pendingUsageQuarantine
	if err := json.Unmarshal(data, &plan); err != nil {
		return UsageReconciliation{}, false, err
	}
	if err := s.validatePendingQuarantineLocked(plan); err != nil {
		return UsageReconciliation{}, false, err
	}
	srcInfo, err := checkRegularOrMissing(plan.Source, "quarantine source")
	if err != nil {
		return UsageReconciliation{}, false, err
	}
	dstInfo, err := checkRegularOrMissing(plan.Dest, "quarantine destination")
	if err != nil {
		return UsageReconciliation{}, false, err
	}
	if srcInfo != nil && dstInfo == nil {
		// Interrupted before moving the original artifact; roll back staged files.
		if plan.StagedHeader != "" {
			_ = os.Remove(plan.StagedHeader)
		}
		if err := os.Remove(planPath); err != nil && !os.IsNotExist(err) {
			return UsageReconciliation{}, false, err
		}
		return UsageReconciliation{}, false, syncRecoveryDirectory(filepath.Dir(planPath))
	}
	if dstInfo == nil {
		return UsageReconciliation{}, false, fmt.Errorf("pending quarantine plan %s missing quarantined artifact %s", planPath, plan.Dest)
	}
	if plan.StagedHeader != "" {
		stagedInfo, err := checkRegularOrMissing(plan.StagedHeader, "staged header")
		if err != nil {
			return UsageReconciliation{}, false, err
		}
		if srcInfo == nil {
			if stagedInfo != nil {
				if err := os.Rename(plan.StagedHeader, plan.Source); err != nil {
					return UsageReconciliation{}, false, err
				}
				if err := syncRecoveryDirectory(filepath.Dir(plan.Source)); err != nil {
					return UsageReconciliation{}, false, err
				}
			} else {
				if plan.CarryForward == nil {
					return UsageReconciliation{}, false, fmt.Errorf("pending quarantine plan %s missing replacement header and carry-forward data", planPath)
				}
				header := UsageAttempt{
					Version:       1,
					LegacyHistory: plan.LegacyHistory,
					CarryForward:  plan.CarryForward,
				}
				if err := writePrivateJSON(plan.Source, header); err != nil {
					return UsageReconciliation{}, false, err
				}
			}
		} else if stagedInfo != nil {
			_ = os.Remove(plan.StagedHeader)
			_ = syncRecoveryDirectory(filepath.Dir(plan.StagedHeader))
		}
	}
	return s.finishQuarantineLocked(plan, planPath, holdingMgmt)
}

func (s *Store) auditUsageActionLocked(action string, report UsageReconciliation) error {
	return s.auditUsageActionWithMgmtLocked(action, report, false)
}

func (s *Store) auditUsageActionWithMgmtLocked(action string, report UsageReconciliation, holdingMgmt bool) error {
	return s.auditWithOptionsLocked(struct {
		Kind   string              `json:"kind"`
		Action string              `json:"action"`
		Report UsageReconciliation `json:"report"`
	}{"usage_reconciliation", action, report}, holdingMgmt, true)
}

func writePrivateJSONNoRename(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		os.Remove(path)
		return errors.Join(writeErr, syncErr, closeErr)
	}
	return nil
}

func writePrivateJSON(path string, v any) error {
	temp := path + ".tmp-" + rand.Text()
	if err := writePrivateJSONNoRename(temp, v); err != nil {
		return err
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

// BlockingUsageError is the strict-mode failure. It names each artifact and the
// repair that clears it, so a repeated cap failure is actionable.
func BlockingUsageError(artifacts []UsageArtifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	return fmt.Errorf("strict metering blocked by %s; repair: harness usage-repair --root <project> --session <id> --quarantine <artifact>", formatUsageArtifacts(artifacts))
}

// IncompleteUsageError formats an actionable strict-metering failure when no
// unquarantined artifact remains, naming any quarantined journal and the way out.
func IncompleteUsageError(sessionID, root string, totals UsageTotals) string {
	if root == "" {
		root = "<project>"
	}
	repairCmd := fmt.Sprintf("harness usage-repair --root %s --session %s --accept-lower-bound", root, sessionID)
	if totals.QuarantinedFile != "" {
		return fmt.Sprintf("persisted session usage is a lower bound after quarantining %s (%d tokens carried forward); acknowledge lower-bound accounting with %q, or set strictMetering: false / maxSessionTokens: 0", totals.QuarantinedFile, totals.Usage.TotalTokens, repairCmd)
	}
	var reasons []string
	if totals.LegacyHistory {
		reasons = append(reasons, "legacy_history")
	}
	if totals.LowerBound {
		reasons = append(reasons, "lower_bound")
	}
	if totals.UnreportedAttempts > 0 {
		reasons = append(reasons, fmt.Sprintf("unreported=%d", totals.UnreportedAttempts))
	}
	if totals.PartialAttempts > 0 {
		reasons = append(reasons, fmt.Sprintf("partial=%d", totals.PartialAttempts))
	}
	if totals.InvalidAttempts > 0 {
		reasons = append(reasons, fmt.Sprintf("invalid=%d", totals.InvalidAttempts))
	}
	if totals.UnpricedAttempts() > 0 {
		reasons = append(reasons, fmt.Sprintf("unpriced=%d", totals.UnpricedAttempts()))
	}
	detail := "incomplete usage"
	if len(reasons) > 0 {
		detail = strings.Join(reasons, ", ")
	}
	return fmt.Sprintf("persisted session usage is incomplete (%s); acknowledge lower-bound accounting with %q, or set strictMetering: false / start a new session", detail, repairCmd)
}
