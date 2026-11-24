package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	return acquireWriterLock(filepath.Join(Dir(), ".writers"), id, true)
}

func acquireWriterIn(dir, id string) (*os.File, error) {
	return acquireWriterLock(dir, id, false)
}

func acquireWriterLock(dir, id string, diagnostics bool) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".lock")
	if err := checkTranscriptPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	actual, statErr := f.Stat()
	named, nameErr := os.Lstat(path)
	if statErr != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(actual, named) {
		f.Close()
		return nil, fmt.Errorf("session writer lease must be a regular, unaliased file")
	}
	if err := checkTranscriptLinks(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	const maxAttempts = 3
	var lockErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		lockErr = tryLockWriter(f)
		if lockErr == nil || !errors.Is(lockErr, ErrSessionBusy) {
			break
		}
		if attempt+1 < maxAttempts {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if lockErr != nil {
		var owner string
		if diagnostics && errors.Is(lockErr, ErrSessionBusy) {
			owner = readLockOwner(f)
		}
		f.Close()
		if owner != "" {
			return nil, fmt.Errorf("session %q: %w (%s)", id, lockErr, owner)
		}
		return nil, fmt.Errorf("session %q: %w", id, lockErr)
	}
	if diagnostics {
		writeLockOwner(f)
	}
	return f, nil
}

func writeLockOwner(f *os.File) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
}

func readLockOwner(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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
