package session

import (
	"fmt"
	"os"
	"path/filepath"

	"minimalharness/internal/privatefile"
)

// ErrSessionBusy reports a transcript already owned by another Store/process.
var ErrSessionBusy = fmt.Errorf("session is already open for writing")

// Keep these files after Close/delete: unlinking a lock would let a second
// process lock a new inode while the first still owns the old one. Literal ID
// filenames preserve the session filesystem's case-alias semantics.
func acquireWriter(id string) (*os.File, error) {
	if err := ValidateSessionID(id); err != nil {
		return nil, err
	}
	return acquireWriterFile(id)
}

func acquireWriterFile(id string) (*os.File, error) {
	dir := filepath.Join(Dir(), ".writers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := privatefile.Open(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := tryLockWriter(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("session %q: %w", id, err)
	}
	return f, nil
}

// Owns reports whether this Store already holds the writer lease for id,
// including a case alias on filesystems that resolve it to the same file.
func (s *Store) Owns(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil || ValidateSessionID(id) != nil {
		return false
	}
	current, err := s.file.Stat()
	if err != nil {
		return false
	}
	target, err := os.Stat(filepath.Join(Dir(), id+".jsonl"))
	return err == nil && os.SameFile(current, target)
}

// Aliased transcript files cannot have independent name leases. Only harness-
// owned regular files with one link may be opened for writing; Load stays read-only.
func checkTranscriptPath(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("session transcript must be a regular file without symlinks")
	}
	return nil
}
