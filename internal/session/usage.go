package session

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"minimalharness/internal/llm"
)

// ErrInvalidProviderUsage means a provider report was quarantined as unknown.
// It is not journal corruption: the record was written, with the counters omitted.
var ErrInvalidProviderUsage = errors.New("invalid or overflowing provider usage recorded as unknown")

// ErrUsageHealthy is returned when quarantining a usage journal that loads cleanly
// without an explicit override.
var ErrUsageHealthy = errors.New("usage journal is healthy")

// SetUsageAppendFault installs a test fault that fails an append after the
// record is sealed and before it is written. Nil restores normal writes.
func SetUsageAppendFault(fault func() error) { usageAppendFault = fault }

var usageAppendFault func() error

// UsageCarryForward preserves verified prefix totals when a damaged or
// overridden usage journal is quarantined, so session caps never reset to zero.
// It is stored on the header record (ID == "") of the replacement journal.
type UsageCarryForward struct {
	Usage              llm.Usage `json:"usage"`
	Attempts           int       `json:"attempts,omitempty"`
	InvalidAttempts    int       `json:"invalid_attempts,omitempty"`
	UnreportedAttempts int       `json:"unreported_attempts,omitempty"`
	PartialAttempts    int       `json:"partial_attempts,omitempty"`
	PricedAttempts     int       `json:"priced_attempts,omitempty"`
	RejectedAttempts   int       `json:"rejected_attempts,omitempty"`
	TransientAttempts  int       `json:"transient_attempts,omitempty"`
	EstimateUSD        float64   `json:"estimate_usd,omitempty"`
	ExactEstimateUSD   string    `json:"exact_estimate_usd,omitempty"`
	QuarantinedFile    string    `json:"quarantined_file"`
	QuarantinedAt      time.Time `json:"quarantined_at"`
	LowerBound         bool      `json:"lower_bound,omitempty"`
	LowerBoundAccepted bool      `json:"lower_bound_accepted,omitempty"`
	DamageReason       string    `json:"damage_reason,omitempty"`
}

// UsageAttempt is a content-free ledger entry for one actual provider request.
// Nil Usage records missing metering, never a fabricated zero-token request.
type UsageAttempt struct {
	Version          int                `json:"version"`
	ID               string             `json:"id"`
	Operation        string             `json:"operation"`
	Attempt          int                `json:"attempt"`
	Request          llm.RequestTrace   `json:"request"`
	EstimateUSD      *float64           `json:"estimate_usd,omitempty"`
	ExactEstimateUSD string             `json:"exact_estimate_usd,omitempty"`
	RateSource       string             `json:"rate_source,omitempty"`
	InvalidUsage     bool               `json:"invalid_usage,omitempty"`
	LegacyHistory    bool               `json:"legacy_history,omitempty"`
	CarryForward     *UsageCarryForward `json:"carry_forward,omitempty"`
	AcceptLowerBound bool               `json:"accept_lower_bound,omitempty"`
}

type acceptedBaseline struct {
	unreportedAttempts int
	partialAttempts    int
	invalidAttempts    int
	unpricedAttempts   int
}

type UsageTotals struct {
	Usage              llm.Usage `json:"usage"`
	Attempts           int       `json:"attempts"`
	InvalidAttempts    int       `json:"invalid_attempts"`
	UnreportedAttempts int       `json:"unreported_attempts"`
	PartialAttempts    int       `json:"partial_attempts"`
	PricedAttempts     int       `json:"priced_attempts"`
	RejectedAttempts   int       `json:"rejected_attempts,omitempty"`
	TransientAttempts  int       `json:"transient_attempts,omitempty"`
	EstimateUSD        float64   `json:"estimate_usd"`
	RecoveryGap        bool      `json:"recovery_gap"`
	LegacyHistory      bool      `json:"legacy_history"`
	LowerBound         bool      `json:"lower_bound,omitempty"`
	QuarantinedFile    string    `json:"quarantined_file,omitempty"`
	LowerBoundAccepted bool      `json:"lower_bound_accepted,omitempty"`
	accepted           acceptedBaseline
	exactEstimateUSD   *big.Rat // replaced on each update so snapshots stay immutable
}

func (t *UsageTotals) addCost(cost *big.Rat) error {
	if cost == nil {
		return nil
	}
	sum := new(big.Rat).Set(cost)
	if t.exactEstimateUSD != nil {
		sum.Add(sum, t.exactEstimateUSD)
	}
	amount, _ := sum.Float64()
	if math.IsInf(amount, 0) {
		return fmt.Errorf("usage cost overflow")
	}
	t.exactEstimateUSD = sum
	t.EstimateUSD = amount
	return nil
}

func (t *UsageTotals) markLowerBoundAccepted() {
	t.LowerBoundAccepted = true
	t.accepted = acceptedBaseline{
		unreportedAttempts: t.UnreportedAttempts,
		partialAttempts:    t.PartialAttempts,
		invalidAttempts:    t.InvalidAttempts,
		unpricedAttempts:   t.UnpricedAttempts(),
	}
}

func (t *UsageTotals) addCarryForward(cf *UsageCarryForward) error {
	if err := cf.validate(); err != nil {
		return err
	}
	if !t.Usage.CanAdd(cf.Usage) {
		return fmt.Errorf("invalid or overflowing carry-forward usage counters")
	}
	cost, err := cf.exactCost()
	if err != nil {
		return err
	}
	t.Usage.Add(cf.Usage)
	t.Attempts += cf.Attempts
	t.InvalidAttempts += cf.InvalidAttempts
	t.UnreportedAttempts += cf.UnreportedAttempts
	t.PartialAttempts += cf.PartialAttempts
	t.PricedAttempts += cf.PricedAttempts
	t.RejectedAttempts += cf.RejectedAttempts
	t.TransientAttempts += cf.TransientAttempts
	if err := t.addCost(cost); err != nil {
		return err
	}
	t.LowerBound = t.LowerBound || cf.LowerBound
	t.QuarantinedFile = cf.QuarantinedFile
	if cf.LowerBoundAccepted {
		t.markLowerBoundAccepted()
	}
	return nil
}

func (t *UsageTotals) add(a UsageAttempt) error {
	if a.ID == "" {
		t.LegacyHistory = t.LegacyHistory || a.LegacyHistory
		if a.CarryForward != nil {
			return t.addCarryForward(a.CarryForward)
		}
		return nil
	}
	if a.AcceptLowerBound {
		t.markLowerBoundAccepted()
		return nil
	}
	cost, err := a.exactCost()
	if err != nil {
		return err
	}
	t.Attempts++
	if a.InvalidUsage {
		t.InvalidAttempts++
	}
	if a.Request.RejectedBeforeInference() {
		t.RejectedAttempts++
	} else if a.Request.Usage == nil {
		t.UnreportedAttempts++
		if a.Request.TransientFailure() {
			t.TransientAttempts++
		}
	} else {
		t.Usage.Add(*a.Request.Usage)
		if !a.Request.TokensComplete {
			t.PartialAttempts++
		}
	}
	if cost != nil {
		if err := t.addCost(cost); err != nil {
			return err
		}
		t.PricedAttempts++
	}
	return nil
}

func parseUsageCost(estimate *float64, raw string, rejected bool) (*big.Rat, error) {
	if estimate == nil && raw == "" {
		return nil, nil
	}
	if estimate == nil || *estimate < 0 || math.IsNaN(*estimate) || math.IsInf(*estimate, 0) || rejected {
		return nil, fmt.Errorf("invalid usage cost")
	}
	if raw == "" {
		// Older ledgers store per-attempt float estimates. Interpret each
		// estimate as a decimal before summing it, as turn budgets do.
		cost, _ := new(big.Rat).SetString(strconv.FormatFloat(*estimate, 'g', -1, 64))
		return cost, nil
	}
	if len(raw) > 2048 || strings.ContainsFunc(raw, func(r rune) bool { return r != '/' && (r < '0' || r > '9') }) {
		return nil, fmt.Errorf("invalid exact usage cost")
	}
	cost, ok := new(big.Rat).SetString(raw)
	if !ok || cost.RatString() != raw {
		return nil, fmt.Errorf("invalid exact usage cost")
	}
	if amount, _ := cost.Float64(); amount != *estimate {
		return nil, fmt.Errorf("exact usage cost does not match estimate")
	}
	return cost, nil
}

func (a UsageAttempt) exactCost() (*big.Rat, error) {
	return parseUsageCost(a.EstimateUSD, a.ExactEstimateUSD, a.Request.RejectedBeforeInference())
}

func (cf UsageCarryForward) exactCost() (*big.Rat, error) {
	if cf.EstimateUSD == 0 && cf.ExactEstimateUSD == "" {
		return nil, nil
	}
	est := cf.EstimateUSD
	return parseUsageCost(&est, cf.ExactEstimateUSD, false)
}

func (cf UsageCarryForward) validate() error {
	if cf.QuarantinedFile == "" {
		return fmt.Errorf("carry-forward record missing quarantined file")
	}
	if !cf.Usage.Valid() {
		return fmt.Errorf("invalid carry-forward usage counters")
	}
	for _, n := range []int{cf.Attempts, cf.InvalidAttempts, cf.UnreportedAttempts, cf.PartialAttempts, cf.PricedAttempts, cf.RejectedAttempts, cf.TransientAttempts} {
		if n < 0 {
			return fmt.Errorf("negative carry-forward attempt count")
		}
	}
	if cf.InvalidAttempts > cf.Attempts || cf.UnreportedAttempts > cf.Attempts || cf.PartialAttempts > cf.Attempts || cf.PricedAttempts+cf.RejectedAttempts > cf.Attempts || cf.TransientAttempts > cf.UnreportedAttempts {
		return fmt.Errorf("inconsistent carry-forward attempt counts")
	}
	if _, err := cf.exactCost(); err != nil {
		return err
	}
	return nil
}

func exactCostFields(cost *big.Rat) (float64, string) {
	if cost == nil || cost.Sign() == 0 {
		return 0, ""
	}
	amount, _ := cost.Float64()
	raw := cost.RatString()
	if len(raw) > 2048 {
		return amount, ""
	}
	return amount, raw
}

func (s *Store) usagePathLocked() string {
	id := s.ID
	if s.header != nil && s.header.OutputID != "" {
		id = s.header.OutputID
	}
	return filepath.Join(Dir(), id, "usage.jsonl")
}

func (s *Store) transcriptHasMessagesLocked() (bool, error) {
	r, err := os.Open(s.Path)
	if err != nil {
		return false, err
	}
	defer r.Close()
	hasMessages := false
	_, _, _, err = scanRecords(r, func(_ []byte, m llm.Message) {
		if m.Role != headerRole {
			hasMessages = true
		}
	})
	return hasMessages, err
}

// loadUsageLocked validates/reconciles the journal once per writer acquisition.
// The transcript can be compacted or rolled back without erasing spent tokens.
func (s *Store) loadUsageLocked() error {
	if s.usageLoaded {
		return nil
	}
	f, err := s.openUsageLocked(false)
	if os.IsNotExist(err) {
		// Existing histories predate metering. Keep that coverage gap visible.
		s.usageTotals = UsageTotals{}
		hasMessages, err := s.transcriptHasMessagesLocked()
		if err != nil {
			return err
		}
		s.usageTotals.LegacyHistory = hasMessages
		pending, err := s.pendingUsageIDsLocked()
		if err != nil {
			return err
		}
		backups, err := recoveryBackups(s.usagePathLocked())
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		artifacts := usageArtifactsLocked(s.usagePathLocked(), pending, backups)
		s.usageTotals.RecoveryGap = len(artifacts) > 0
		s.usageRecovery = UsageReconciliation{SessionID: s.ID, Artifacts: artifacts, RecoveryGap: s.usageTotals.RecoveryGap}
		s.usageLoaded = true
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := checkTranscriptLinks(f); err != nil {
		return err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("usage journal must be a regular file: %v", err)
	}
	if err := recoverTail(f); err != nil {
		return s.damagedJournal(err.Error())
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	backups, err := recoveryBackups(f.Name())
	if err != nil {
		return err
	}
	pending, err := s.pendingUsageIDsLocked()
	if err != nil {
		return err
	}
	scan, err := scanUsageJournal(f, func(a UsageAttempt) error {
		if a.ID != "" && pending[a.ID] {
			if err := s.clearPendingUsageLocked(a.ID); err != nil {
				return err
			}
			delete(pending, a.ID)
			s.rememberCommitted(a.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if scan.damaged {
		return s.damagedJournal(scan.damageReason)
	}
	// A marker removed before dispatch is not missing usage. Clear it on open
	// so a crash cannot leave RecoveryGap set for a request that never left.
	for id := range pending {
		noted, err := s.notDispatchedUsageLocked(id)
		if err != nil {
			return err
		}
		if !noted {
			continue
		}
		if err := s.clearPendingUsageLocked(id); err != nil {
			return err
		}
		delete(pending, id)
	}
	if !scan.hasHeader && !scan.hasNonHeader {
		// An empty file is not history. It is created before the first request
		// and must not look like a session that predates metering.
		if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
			scan.totals.LegacyHistory = true
		}
	}
	s.usageChainOn = scan.chainStarted
	s.usageChainDigest = scan.lastDigest
	if scan.chainStarted {
		s.usageChainSeq = scan.sequences
	} else if scan.hasHeader && !scan.hasNonHeader {
		// A header written before the first attempt (including carry-forward) is
		// not a legacy journal. Keep chaining so the next record is sequence 1.
		s.usageChainOn = true
		s.usageChainSeq = 0
		s.usageChainDigest = ""
	}
	artifacts := usageArtifactsLocked(f.Name(), pending, backups)
	scan.totals.RecoveryGap = len(artifacts) > 0
	s.usageRecovery = UsageReconciliation{
		SessionID:      s.ID,
		Artifacts:      artifacts,
		RecoveryGap:    scan.totals.RecoveryGap,
		ChainSequences: s.usageChainSeq,
		CarryForward:   scan.carryForward,
	}
	s.usageInfo, err = f.Stat()
	if err != nil {
		return err
	}
	s.usageTotals = scan.totals
	s.usageLoaded = true
	return nil
}

func (s *Store) UsageTotals() (UsageTotals, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A readable journal does not prove an append that failed reached disk.
	// Keep this writer's accounting unavailable, even if reconciliation can read
	// its previous valid prefix. Loading a different writer starts a new run.
	if s.usageWriteFailed {
		if !s.usageFailureReported {
			s.usageFailureReported = true
			return UsageTotals{}, fmt.Errorf("provider usage persistence failed; accounting incomplete for this writer")
		}
		if err := s.reopenUsageAfterWriteFailureLocked(); err != nil {
			return UsageTotals{}, err
		}
	}
	if s.closed {
		return UsageTotals{}, os.ErrClosed
	}
	if err := s.loadUsageLocked(); err != nil {
		return UsageTotals{}, err
	}
	return s.usageTotals, nil
}

// RecordUsage syncs each attempt before the next provider request or tool action.
// A journal failure stops the turn; it cannot silently discard billing evidence.
func (s *Store) RecordUsage(a UsageAttempt) (writeErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recorded := false
	defer func() {
		if writeErr != nil && !recorded {
			s.usageLoaded = false
			s.usageWriteFailed = true
			s.usageFailureReported = false
		}
	}()
	if s.usageWriteFailed {
		if err := s.reopenUsageAfterWriteFailureLocked(); err != nil {
			return err
		}
	}
	if s.closed {
		return os.ErrClosed
	}
	if err := s.loadUsageLocked(); err != nil {
		return err
	}
	a.Version = 1
	if a.ID == "" || a.Request.ID != a.ID {
		return fmt.Errorf("invalid usage attempt identity")
	}
	// Keep only accounting fields. Request hashes/message arrays are diagnostics,
	// and never grow the durable ledger with the entire request's shape.
	if a.Request.Usage != nil && (!a.Request.Usage.Valid() || !s.usageTotals.Usage.CanAdd(*a.Request.Usage)) {
		a.InvalidUsage = true
		a.Request.Usage = nil
		a.Request.TokensComplete = false
		a.Request.PriceCountsComplete = false
		a.EstimateUSD = nil
		a.ExactEstimateUSD = ""
		a.RateSource = ""
	}
	if a.InvalidUsage && (a.Request.Usage != nil || a.EstimateUSD != nil || a.ExactEstimateUSD != "") {
		return fmt.Errorf("invalid usage marker")
	}
	nextTotals := s.usageTotals
	if err := nextTotals.add(a); err != nil {
		return err
	}
	tr := a.Request
	a.Request = llm.RequestTrace{ID: tr.ID, StartedAt: tr.StartedAt, Model: tr.Model, API: tr.API, EndpointHash: tr.EndpointHash, DurationMS: tr.DurationMS, Outcome: tr.Outcome, Usage: tr.Usage, TokensComplete: tr.TokensComplete, PriceCountsComplete: tr.PriceCountsComplete, StatusCode: tr.StatusCode}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := s.appendJournalLineLocked(data, s.usageTotals.LegacyHistory); err != nil {
		return err
	}
	if err := s.clearPendingUsageLocked(a.ID); err != nil {
		return err
	}
	if s.usageRecovery.Artifacts != nil {
		kept := s.usageRecovery.Artifacts[:0]
		for _, artifact := range s.usageRecovery.Artifacts {
			if !strings.HasSuffix(artifact.Path, usagePendingPrefix+a.ID) {
				kept = append(kept, artifact)
			}
		}
		s.usageRecovery.Artifacts = kept
		s.usageRecovery.RecoveryGap = len(kept) > 0
		nextTotals.RecoveryGap = s.usageRecovery.RecoveryGap
	}
	s.usageTotals = nextTotals
	recorded = true
	if a.InvalidUsage {
		return ErrInvalidProviderUsage
	}
	return nil
}

func (s *Store) appendJournalLineLocked(data []byte, legacyHistory bool) error {
	if len(data) >= 64<<10 {
		return fmt.Errorf("usage record exceeds limit")
	}
	path := s.usagePathLocked()
	f, err := s.openUsageLocked(true)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("usage journal must be a regular file")
	}
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if info.Size() == 0 {
		header, err := json.Marshal(UsageAttempt{Version: 1, LegacyHistory: legacyHistory})
		if err != nil {
			return err
		}
		if _, err := f.Write(append(header, '\n')); err != nil {
			return err
		}
		// The header is not an attempt. The next record starts the chain.
		s.usageChainSeq = 0
		s.usageChainOn = true
		s.usageChainDigest = ""
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	// Journals that already contain unchained records stay unchained. Sealing
	// only the new tail would look like a deleted middle record on reopen.
	// info.Size is the size before the header write, so a brand-new journal is
	// zero here even after the header lands.
	if s.usageChainOn || info.Size() == 0 {
		sealed, _, err := sealUsageRecord(data, s.usageChainSeq+1, s.usageChainDigest)
		if err != nil {
			return err
		}
		data = sealed
	}
	if usageAppendFault != nil {
		if err := usageAppendFault(); err != nil {
			// The bytes were not written. Reload will see the previous prefix.
			s.usageLoaded = false
			s.usageInfo = nil
			return err
		}
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := syncRecoveryDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	s.usageInfo, err = f.Stat()
	if err != nil {
		return err
	}
	if s.usageChainOn || info.Size() == 0 {
		s.usageChainSeq++
		s.usageChainDigest = hashUsageRecord(append(append([]byte(nil), data...), '\n'))
		s.usageChainOn = true
	}
	return nil
}

// AcceptUsageLowerBound records an audited acknowledgment of a historical
// metering gap or carried-forward lower bound so strict metering can proceed
// while enforcing session caps against the recorded subtotal. Any remaining
// unquarantined artifact or subsequent incomplete attempt blocks again.
func (s *Store) AcceptUsageLowerBound() (totals UsageTotals, changed bool, acceptErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recorded := false
	defer func() {
		if acceptErr != nil && !recorded {
			s.usageLoaded = false
			s.usageWriteFailed = true
			s.usageFailureReported = false
		}
	}()
	if s.usageWriteFailed {
		if err := s.reopenUsageAfterWriteFailureLocked(); err != nil {
			return UsageTotals{}, false, err
		}
	}
	if s.closed {
		return UsageTotals{}, false, os.ErrClosed
	}
	if err := s.loadUsageLocked(); err != nil {
		return UsageTotals{}, false, err
	}
	if s.usageTotals.RecoveryGap {
		return UsageTotals{}, false, BlockingUsageError(s.usageRecovery.Artifacts)
	}
	if (s.usageTotals.TokenCountsComplete() && s.usageTotals.UnpricedAttempts() == 0) || s.usageTotals.LowerBoundBaselineReady() {
		recorded = true
		return s.usageTotals, false, nil
	}
	id := rand.Text()
	ack := UsageAttempt{
		Version:          1,
		ID:               id,
		Operation:        "usage_repair_accept_lower_bound",
		Request:          llm.RequestTrace{ID: id},
		AcceptLowerBound: true,
	}
	nextTotals := s.usageTotals
	if err := nextTotals.add(ack); err != nil {
		return UsageTotals{}, false, err
	}
	data, err := json.Marshal(ack)
	if err != nil {
		return UsageTotals{}, false, err
	}
	if err := s.appendJournalLineLocked(data, s.usageTotals.LegacyHistory); err != nil {
		return UsageTotals{}, false, err
	}
	s.usageTotals = nextTotals
	s.usageRecovery.LowerBoundAccepted = true
	s.usageRecovery.ChainSequences = s.usageChainSeq
	recorded = true
	if err := s.auditUsageActionLocked("accept_lower_bound", s.usageRecovery); err != nil {
		return UsageTotals{}, true, err
	}
	return s.usageTotals, true, nil
}

// Resolve only within the session root and reject aliasing of another owner's
// artifact family before opening the nonblocking regular-file journal.
func (s *Store) openUsageLocked(create bool) (*os.File, error) {
	path := s.usagePathLocked()
	root, err := os.OpenRoot(Dir())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	family := filepath.Base(filepath.Dir(path))
	if create {
		if err := root.Mkdir(family, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if err := syncRecoveryDirectory(Dir()); err != nil {
			return nil, err
		}
	}
	info, err := root.Lstat(family)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("usage artifact family must be a directory without symlinks")
	}
	rel := filepath.Join(family, "usage.jsonl")
	current, statErr := root.Lstat(rel)
	if s.usageInfo != nil && (statErr != nil || !os.SameFile(s.usageInfo, current) || current.Size() != s.usageInfo.Size() || !current.ModTime().Equal(s.usageInfo.ModTime())) {
		// A hot writer must never recreate/rebase a vanished or replaced ledger.
		// Persist the discontinuity separately before reporting it, so a cold
		// writer still sees the missing history even when the prefix is gone.
		pending, markerErr := root.OpenFile(filepath.Join(family, usagePendingPrefix+rand.Text()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if markerErr == nil {
			markerErr = pending.Sync()
			markerErr = errors.Join(markerErr, pending.Close(), syncRecoveryDirectory(filepath.Dir(path)))
		}
		return nil, errors.Join(fmt.Errorf("usage journal changed outside this writer"), markerErr)
	}
	if statErr == nil {
		if !current.Mode().IsRegular() {
			return nil, fmt.Errorf("usage journal must be a regular file without symlinks")
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	flags := os.O_RDWR | imageBlobReadFlags
	if create {
		flags |= os.O_CREATE
	}
	f, err := root.OpenFile(rel, flags, 0600)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("invalid usage journal: %v", err)
	}
	if err := checkTranscriptLinks(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// TokenCountsComplete describes measured coverage separately from whether the
// writer/journal is available. A recovery backup is durable evidence that a
// final attempt was lost, even after its invalid tail has been truncated.
func (t UsageTotals) TokenCountsComplete() bool {
	return !t.LegacyHistory && !t.RecoveryGap && !t.LowerBound && t.UnreportedAttempts == 0 && t.PartialAttempts == 0 && t.InvalidAttempts == 0
}

// LowerBoundTokensAccepted reports whether historical token metering gaps have
// been explicitly acknowledged and no unquarantined gap or newer incomplete
// attempt occurred since that acknowledgment.
func (t UsageTotals) LowerBoundTokensAccepted() bool {
	if !t.LowerBoundAccepted || t.RecoveryGap {
		return false
	}
	return t.UnreportedAttempts == t.accepted.unreportedAttempts &&
		t.PartialAttempts == t.accepted.partialAttempts &&
		t.InvalidAttempts == t.accepted.invalidAttempts
}

// LowerBoundCostAccepted reports whether historical cost metering gaps have
// been explicitly acknowledged and no unquarantined gap or newer unpriced or
// incomplete attempt occurred since that acknowledgment.
func (t UsageTotals) LowerBoundCostAccepted() bool {
	return t.LowerBoundTokensAccepted() && t.UnpricedAttempts() == t.accepted.unpricedAttempts
}

// LowerBoundBaselineReady reports whether both historical token and cost gaps
// are covered by the latest lower-bound acknowledgment.
func (t UsageTotals) LowerBoundBaselineReady() bool {
	return t.LowerBoundCostAccepted()
}

// UnpricedAttempts counts requests whose cost is unknown.
func (t UsageTotals) UnpricedAttempts() int {
	return t.Attempts - t.PricedAttempts - t.RejectedAttempts
}

// ExactCost returns the recorded subtotal as an independent value.
func (t UsageTotals) ExactCost() *big.Rat {
	cost := new(big.Rat)
	if t.exactEstimateUSD != nil {
		cost.Set(t.exactEstimateUSD)
	}
	return cost
}

func (s *Store) rememberCommitted(id string) {
	for _, existing := range s.usageCommitted {
		if existing == id {
			return
		}
	}
	s.usageCommitted = append(s.usageCommitted, id)
}

func usageArtifactsLocked(journalPath string, pending map[string]bool, backups []string) []UsageArtifact {
	var artifacts []UsageArtifact
	for id := range pending {
		artifacts = append(artifacts, UsageArtifact{
			Path:   filepath.Join(filepath.Dir(journalPath), usagePendingPrefix+id),
			Kind:   "pending_marker",
			Reason: "unmatched crash or discontinuity marker; usage may have been spent",
		})
	}
	for _, path := range backups {
		artifacts = append(artifacts, UsageArtifact{
			Path: path, Kind: "recovery_backup", Reason: "torn tail retained as evidence; RecoveryGap stays until quarantine",
		})
	}
	slices.SortFunc(artifacts, func(a, b UsageArtifact) int { return strings.Compare(a.Path, b.Path) })
	return artifacts
}

// reopenUsageAfterWriteFailureLocked keeps the failed attempt's evidence and
// lets a later call use a fresh descriptor. The pending marker, or a torn tail
// recovered on the next open, is what proves the attempt was not discarded.
func (s *Store) reopenUsageAfterWriteFailureLocked() error {
	if !s.usageWriteFailed {
		return nil
	}
	s.usageLoaded = false
	s.usageInfo = nil
	s.usageWriteFailed = false
	s.usageFailureReported = false
	if err := s.loadUsageLocked(); err != nil {
		s.usageWriteFailed = true
		s.usageFailureReported = true
		return fmt.Errorf("provider usage persistence failed; accounting incomplete for this writer: %w", err)
	}
	// The failed call may have cleared the loaded flag without refreshing the
	// totals. A successful reopen replaces both.
	return nil
}
