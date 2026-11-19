package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"minimalharness/internal/llm"
	"os"
	"path/filepath"
)

// UsageAttempt is a content-free ledger entry for one actual provider request.
// Nil Usage records missing metering, never a fabricated zero-token request.
type UsageAttempt struct {
	Version       int              `json:"version"`
	ID            string           `json:"id"`
	Operation     string           `json:"operation"`
	Attempt       int              `json:"attempt"`
	Request       llm.RequestTrace `json:"request"`
	EstimateUSD   *float64         `json:"estimate_usd,omitempty"`
	RateSource    string           `json:"rate_source,omitempty"`
	LegacyHistory bool             `json:"legacy_history,omitempty"`
}

type UsageTotals struct {
	Usage              llm.Usage `json:"usage"`
	Attempts           int       `json:"attempts"`
	UnreportedAttempts int       `json:"unreported_attempts"`
	PartialAttempts    int       `json:"partial_attempts"`
	PricedAttempts     int       `json:"priced_attempts"`
	EstimateUSD        float64   `json:"estimate_usd"`
	LegacyHistory      bool      `json:"legacy_history"`
}

func (t *UsageTotals) add(a UsageAttempt) {
	if a.ID == "" {
		t.LegacyHistory = t.LegacyHistory || a.LegacyHistory
		return
	}
	t.Attempts++
	if a.Request.Usage == nil {
		t.UnreportedAttempts++
	} else {
		t.Usage.Add(*a.Request.Usage)
		if !a.Request.TokensComplete {
			t.PartialAttempts++
		}
	}
	if a.EstimateUSD != nil {
		t.PricedAttempts++
		t.EstimateUSD += *a.EstimateUSD
	}
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
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	var totals UsageTotals
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 64<<10)
	first := true
	for sc.Scan() {
		var a UsageAttempt
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			return fmt.Errorf("invalid usage journal: %w", err)
		}
		if a.Version != 1 || (first && a.ID != "") || (!first && (a.ID == "" || a.Request.ID != a.ID)) {
			return fmt.Errorf("invalid usage journal record")
		}
		if a.EstimateUSD != nil && (*a.EstimateUSD < 0 || math.IsInf(*a.EstimateUSD, 0) || math.IsNaN(*a.EstimateUSD)) {
			return fmt.Errorf("invalid usage cost")
		}
		totals.add(a)
		first = false
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if first {
		totals.LegacyHistory = true
	}
	s.usageTotals = totals
	s.usageLoaded = true
	return nil
}

func (s *Store) UsageTotals() (UsageTotals, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	defer func() {
		if writeErr != nil {
			s.usageLoaded = false
		}
	}()
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
	tr := a.Request
	a.Request = llm.RequestTrace{ID: tr.ID, StartedAt: tr.StartedAt, Model: tr.Model, API: tr.API, EndpointHash: tr.EndpointHash, DurationMS: tr.DurationMS, Outcome: tr.Outcome, Usage: tr.Usage, TokensComplete: tr.TokensComplete, PriceCountsComplete: tr.PriceCountsComplete}
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
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
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
	s.usageTotals.add(a)
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
	}
	info, err := root.Lstat(family)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("usage artifact family must be a directory without symlinks")
	}
	rel := filepath.Join(family, "usage.jsonl")
	if info, err := root.Lstat(rel); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("usage journal must be a regular file without symlinks")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
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
