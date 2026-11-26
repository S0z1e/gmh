package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const usagePendingPrefix = "usage-pending-"

// BeginUsageAttempt persists evidence before dispatch. An interrupted or failed
// append can then never make the missing request disappear on a cold reopen.
func (s *Store) BeginUsageAttempt(id string) (beginErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if beginErr != nil {
			s.usageWriteFailed = true
			s.usageFailedTurn = false
		}
	}()
	if s.closed {
		return os.ErrClosed
	}
	if !validBackupID(id) {
		return fmt.Errorf("invalid pending usage identity")
	}
	if s.usageWriteFailed {
		if err := s.reopenUsageAfterWriteFailureLocked(); err != nil {
			return err
		}
	}
	if err := s.loadUsageLocked(); err != nil {
		return err
	}
	// This also validates/creates the owned family, with durable parent entries.
	f, err := s.openUsageLocked(true)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dir := filepath.Dir(s.usagePathLocked())
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	pending, err := root.OpenFile(usagePendingPrefix+id, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = pending.Sync()
	closeErr := pending.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return syncRecoveryDirectory(dir)
}

func (s *Store) clearPendingUsageLocked(id string) error {
	if !validBackupID(id) {
		return nil
	} // Legacy/direct records never made a marker.
	dir := filepath.Dir(s.usagePathLocked())
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(usagePendingPrefix + id); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncRecoveryDirectory(dir)
}

// CompleteUsageScope clears the wrapper marker when a compaction callback's
// actual streams have already been individually journaled (or no HTTP ran).
func (s *Store) CompleteUsageScope(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	return s.clearPendingUsageLocked(id)
}

func (s *Store) pendingUsageIDsLocked() (map[string]bool, error) {
	ids := map[string]bool{}
	dir := filepath.Dir(s.usagePathLocked())
	f, err := os.Open(dir)
	if os.IsNotExist(err) {
		return ids, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(100001)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 100000 {
		return nil, fmt.Errorf("too many usage artifacts")
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), usagePendingPrefix) {
			continue
		}
		id := strings.TrimPrefix(e.Name(), usagePendingPrefix)
		if !validBackupID(id) {
			return nil, fmt.Errorf("invalid pending usage artifact")
		}
		st, err := e.Info()
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() || st.Size() != 0 {
			return nil, fmt.Errorf("invalid pending usage artifact")
		}
		ids[id] = true
	}
	return ids, nil
}
