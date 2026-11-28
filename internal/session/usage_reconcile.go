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
	if err := s.loadUsageLocked(); err != nil && !errors.Is(err, errUsageDamaged) {
		return UsageReconciliation{}, err
	}
	switch action {
	case usageRepairQuarantine:
		res, err := s.quarantineUsageLocked(artifact, opts)
		if err != nil {
			return UsageReconciliation{}, err
		}
		if res.moved {
			pending, err := s.pendingUsageIDsLocked()
			if err != nil {
				return UsageReconciliation{}, err
			}
			for id := range res.committedIDs {
				s.rememberCommitted(id)
				if pending[id] {
					if err := s.clearPendingUsageLocked(id); err != nil {
						return UsageReconciliation{}, err
					}
				}
			}
			s.usageLoaded = false
			s.usageInfo = nil
			s.usageTotals = UsageTotals{}
			s.usageChainSeq = 0
			s.usageChainDigest = ""
			s.usageChainOn = false
		}
		if err := s.loadUsageLocked(); err != nil && !errors.Is(err, errUsageDamaged) {
			return UsageReconciliation{}, err
		}
		report, err := s.reconcileUsageLocked(action)
		if err != nil {
			return UsageReconciliation{}, err
		}
		if res.moved {
			report.Quarantined = append(report.Quarantined, artifact)
			report.HealthyOverride = res.healthyOverride
			if res.carryForward != nil {
				report.CarryForward = res.carryForward
			}
		}
		s.usageRecovery = report
		if err := s.auditUsageActionLocked(action, report); err != nil {
			return UsageReconciliation{}, err
		}
		return report, nil
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

type quarantineResult struct {
	moved           bool
	healthyOverride bool
	carryForward    *UsageCarryForward
	committedIDs    map[string]bool
}

func (s *Store) quarantineUsageLocked(artifact string, opts UsageRepairOptions) (quarantineResult, error) {
	if artifact == "" {
		return quarantineResult{}, fmt.Errorf("quarantine needs the artifact path named by the strict error")
	}
	clean := filepath.Clean(artifact)
	family := filepath.Dir(s.usagePathLocked())
	rel, err := filepath.Rel(family, clean)
	if err != nil || rel == "." || rel == ".." || strings.Contains(rel, string(os.PathSeparator)) {
		return quarantineResult{}, fmt.Errorf("artifact %s is outside this session usage family", artifact)
	}
	base := filepath.Base(clean)
	allowed := base == "usage.jsonl" || strings.HasPrefix(base, usagePendingPrefix) || strings.Contains(base, ".jsonl-torn-")
	if !allowed || strings.Contains(base, "..") {
		return quarantineResult{}, fmt.Errorf("artifact %s is not a usage journal, marker, or recovery backup", artifact)
	}
	info, err := os.Lstat(clean)
	if os.IsNotExist(err) {
		return quarantineResult{}, nil
	}
	if err != nil {
		return quarantineResult{}, err
	}
	if !info.Mode().IsRegular() {
		return quarantineResult{}, fmt.Errorf("artifact %s is not a regular file", artifact)
	}
	destDir := s.quarantinePathLocked()
	dest := filepath.Join(destDir, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+rand.Text()+"-"+base)
	if base == "usage.jsonl" {
		view, err := s.readUsageJournalLocked()
		if err != nil {
			return quarantineResult{}, err
		}
		if !view.damaged && !opts.ForceHealthy {
			return quarantineResult{}, fmt.Errorf("%w: %s loads cleanly; pass --allow-healthy to override", ErrUsageHealthy, clean)
		}
		legacyHistory := view.totals.LegacyHistory
		if !view.hasHeader {
			if hasMsgs, msgErr := s.transcriptHasMessagesLocked(); msgErr == nil && hasMsgs {
				legacyHistory = true
			}
		}
		estUSD, exactUSD := exactCostFields(view.totals.ExactCost())
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
			DamageReason:       view.damageReason,
		}
		header := UsageAttempt{
			Version:       1,
			LegacyHistory: legacyHistory,
			CarryForward:  cf,
		}
		temp := clean + ".tmp-" + rand.Text()
		if err := writePrivateJSONNoRename(temp, header); err != nil {
			return quarantineResult{}, err
		}
		if err := os.MkdirAll(destDir, 0700); err != nil {
			os.Remove(temp)
			return quarantineResult{}, err
		}
		if err := os.Rename(clean, dest); err != nil {
			os.Remove(temp)
			return quarantineResult{}, err
		}
		if err := syncRecoveryDirectory(destDir); err != nil {
			return quarantineResult{}, err
		}
		if err := os.Rename(temp, clean); err != nil {
			_ = os.Rename(dest, clean)
			os.Remove(temp)
			return quarantineResult{}, err
		}
		if err := syncRecoveryDirectory(family); err != nil {
			return quarantineResult{}, err
		}
		return quarantineResult{
			moved:           true,
			healthyOverride: !view.damaged && opts.ForceHealthy,
			carryForward:    cf,
			committedIDs:    view.ids,
		}, nil
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return quarantineResult{}, err
	}
	if err := os.Rename(clean, dest); err != nil {
		return quarantineResult{}, err
	}
	if err := syncRecoveryDirectory(destDir); err != nil {
		return quarantineResult{}, err
	}
	return quarantineResult{moved: true}, syncRecoveryDirectory(family)
}

func (s *Store) auditUsageActionLocked(action string, report UsageReconciliation) error {
	return s.auditLocked(struct {
		Kind   string              `json:"kind"`
		Action string              `json:"action"`
		Report UsageReconciliation `json:"report"`
	}{"usage_reconciliation", action, report})
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
