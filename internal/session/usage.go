package session

import (
	"bufio"
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

	"minimalharness/internal/llm"
)

// ErrInvalidProviderUsage means a provider report was quarantined as unknown.
// It is not journal corruption: the record was written, with the counters omitted.
var ErrInvalidProviderUsage = errors.New("invalid or overflowing provider usage recorded as unknown")

// SetUsageAppendFault installs a test fault that fails an append after the
// record is sealed and before it is written. Nil restores normal writes.
func SetUsageAppendFault(fault func() error) { usageAppendFault = fault }

var usageAppendFault func() error

// UsageAttempt is a content-free ledger entry for one actual provider request.
// Nil Usage records missing metering, never a fabricated zero-token request.
type UsageAttempt struct {
	Version          int              `json:"version"`
	ID               string           `json:"id"`
	Operation        string           `json:"operation"`
	Attempt          int              `json:"attempt"`
	Request          llm.RequestTrace `json:"request"`
	EstimateUSD      *float64         `json:"estimate_usd,omitempty"`
	ExactEstimateUSD string           `json:"exact_estimate_usd,omitempty"`
	RateSource       string           `json:"rate_source,omitempty"`
	InvalidUsage     bool             `json:"invalid_usage,omitempty"`
	LegacyHistory    bool             `json:"legacy_history,omitempty"`
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
	exactEstimateUSD   *big.Rat  // replaced on each update so snapshots stay immutable
}

func (t *UsageTotals) add(a UsageAttempt) error {
	if a.ID == "" {
		t.LegacyHistory = t.LegacyHistory || a.LegacyHistory
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
		if t.exactEstimateUSD != nil {
			cost.Add(cost, t.exactEstimateUSD)
		}
		amount, _ := cost.Float64()
		if math.IsInf(amount, 0) {
			return fmt.Errorf("usage cost overflow")
		}
		t.exactEstimateUSD = cost
		t.PricedAttempts++
		t.EstimateUSD = amount
	}
	return nil
}

func (a UsageAttempt) exactCost() (*big.Rat, error) {
	if a.EstimateUSD == nil && a.ExactEstimateUSD == "" {
		return nil, nil
	}
	if a.EstimateUSD == nil || *a.EstimateUSD < 0 || math.IsNaN(*a.EstimateUSD) || math.IsInf(*a.EstimateUSD, 0) || a.Request.RejectedBeforeInference() {
		return nil, fmt.Errorf("invalid usage cost")
	}
	if a.ExactEstimateUSD == "" {
		// Older ledgers store per-attempt float estimates. Interpret each
		// estimate as a decimal before summing it, as turn budgets do.
		cost, _ := new(big.Rat).SetString(strconv.FormatFloat(*a.EstimateUSD, 'g', -1, 64))
		return cost, nil
	}
	raw := a.ExactEstimateUSD
	if len(raw) > 2048 || strings.ContainsFunc(raw, func(r rune) bool { return r != '/' && (r < '0' || r > '9') }) {
		return nil, fmt.Errorf("invalid exact usage cost")
	}
	cost, ok := new(big.Rat).SetString(raw)
	if !ok || cost.RatString() != raw {
		return nil, fmt.Errorf("invalid exact usage cost")
	}
	if amount, _ := cost.Float64(); amount != *a.EstimateUSD {
		return nil, fmt.Errorf("exact usage cost does not match estimate")
	}
	return cost, nil
}

func (s *Store) usagePathLocked() string {
	id := s.ID
	if s.header != nil && s.header.OutputID != "" {
		id = s.header.OutputID
	}
	return filepath.Join(Dir(), id, "usage.jsonl")
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
		r, err := os.Open(s.Path)
		if err != nil {
			return err
		}
		defer r.Close()
		_, _, _, err = scanRecords(r, func(_ []byte, m llm.Message) {
			if m.Role != headerRole {
				s.usageTotals.LegacyHistory = true
			}
		})
		if err != nil {
			return err
		}
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
	totals := UsageTotals{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 64<<10)
	first := true
	var previous string
	var want int64 = 1
	var chainStarted bool
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		record, chain, _, chainErr := unwrapUsageRecord(line)
		if chainErr != nil {
			return s.damagedJournal(chainErr.Error())
		}
		if err := linkUsageChain(chain, want, previous, chainStarted); err != nil {
			return s.damagedJournal(err.Error())
		}
		var a UsageAttempt
		if err := json.Unmarshal(record, &a); err != nil {
			return s.damagedJournal(err.Error())
		}
		if a.Version != 1 || (first && a.ID != "") || (!first && (a.ID == "" || a.Request.ID != a.ID)) {
			return s.damagedJournal("invalid usage journal record")
		}
		if chain != nil {
			chainStarted = true
			previous = hashUsageRecord(append(append([]byte(nil), line...), '\n'))
			want = chain.Sequence + 1
		}
		if a.Request.Usage != nil && (!a.Request.Usage.Valid() || !totals.Usage.CanAdd(*a.Request.Usage)) {
			return s.damagedJournal("invalid or overflowing usage counters")
		}
		if a.InvalidUsage && (a.Request.Usage != nil || a.EstimateUSD != nil || a.ExactEstimateUSD != "") {
			return s.damagedJournal("invalid usage marker")
		}
		if err := totals.add(a); err != nil {
			return s.damagedJournal(err.Error())
		}
		if pending[a.ID] {
			if err := s.clearPendingUsageLocked(a.ID); err != nil {
				return err
			}
			delete(pending, a.ID)
			s.rememberCommitted(a.ID)
		}
		first = false
	}
	if err := sc.Err(); err != nil {
		return err
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
	if first {
		// An empty file is not history. It is created before the first request
		// and must not look like a session that predates metering.
		if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
			totals.LegacyHistory = true
		}
	}
	s.usageChainOn = chainStarted
	s.usageChainDigest = previous
	if chainStarted {
		s.usageChainSeq = want - 1
	} else if !first && totals.Attempts == 0 {
		// A header written before the first attempt is not a legacy journal.
		// Keep chaining so the next record is sequence 1.
		s.usageChainOn = true
		s.usageChainSeq = 0
		s.usageChainDigest = ""
	}
	artifacts := usageArtifactsLocked(f.Name(), pending, backups)
	totals.RecoveryGap = len(artifacts) > 0
	s.usageRecovery = UsageReconciliation{SessionID: s.ID, Artifacts: artifacts, RecoveryGap: totals.RecoveryGap, ChainSequences: s.usageChainSeq}
	s.usageInfo, err = f.Stat()
	if err != nil {
		return err
	}
	s.usageTotals = totals
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
		header, err := json.Marshal(UsageAttempt{Version: 1, LegacyHistory: s.usageTotals.LegacyHistory})
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
	if s.usageChainOn || info.Size() == 0 {
		s.usageChainSeq++
		s.usageChainDigest = hashUsageRecord(append(append([]byte(nil), data...), '\n'))
		s.usageChainOn = true
	}
	s.usageTotals = nextTotals
	recorded = true
	if a.InvalidUsage {
		return ErrInvalidProviderUsage
	}
	return nil
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
	return !t.LegacyHistory && !t.RecoveryGap && t.UnreportedAttempts == 0 && t.PartialAttempts == 0 && t.InvalidAttempts == 0
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
