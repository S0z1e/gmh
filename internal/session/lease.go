package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/workspace"
)

// ErrSessionBusy reports a transcript already owned by another Store/process.
var ErrSessionBusy = fmt.Errorf("session is already open for writing")

const (
	lockHeartbeatInterval = 15 * time.Second
	lockStaleTTL          = 90 * time.Second
)

// LockOwner describes the process holding a session writer lock.
type LockOwner struct {
	PID            int    `json:"pid"`
	Started        string `json:"started"`
	Heartbeat      string `json:"heartbeat,omitempty"`
	Host           string `json:"host,omitempty"`
	ProcessStarted string `json:"process_started,omitempty"`
}

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
	f, err := openWriterLease(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
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
			owner = formatLockOwner(f)
		}
		f.Close()
		if owner != "" {
			return nil, fmt.Errorf("session %q: %w (%s)", id, lockErr, owner)
		}
		return nil, fmt.Errorf("session %q: %w", id, lockErr)
	}
	if diagnostics {
		if err := writeLockOwner(f, newLockOwner()); err != nil {
			f.Close()
			return nil, fmt.Errorf("record writer lock owner: %w", err)
		}
	}
	return f, nil
}

func openWriterLease(path string, flags int) (*os.File, error) {
	if err := checkTranscriptPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, flags|metadataReadFlags, 0600)
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
	return f, nil
}

// startWriterHeartbeat periodically refreshes the lock owner's heartbeat until
// the returned stop function is called. The stop function waits for the
// goroutine to exit before returning so the caller can safely close f.
func startWriterHeartbeat(f *os.File) func() {
	if f == nil {
		return func() {}
	}
	owner, ok := parseLockOwner(f)
	if !ok || owner.PID != os.Getpid() {
		owner = newLockOwner()
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(lockHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := writeLockOwner(f, owner); err != nil {
					return
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}

// StealWriterLock recovers a lock for a session bound to root. A live owner
// must be verified on this host and have a stale heartbeat before termination.
func StealWriterLock(id, root string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	identity, err := workspace.Identify(root)
	if err != nil {
		return err
	}
	unlock, err := lockManagement()
	if err != nil {
		return err
	}
	defer unlock()
	id, err = canonicalSessionID(id)
	if err != nil {
		return err
	}
	header, err := readArtifactHeader(filepath.Join(Dir(), id+".jsonl"))
	if err == nil {
		err = checkBinding(header, identity, repositoryIdentity(identity.Path))
	}
	if err != nil {
		return fmt.Errorf("session %q: %w", id, err)
	}
	path := filepath.Join(Dir(), ".writers", id+".lock")
	f, err := openWriterLease(path, os.O_RDWR)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tryLockWriter(f); err == nil {
		return nil
	} else if !errors.Is(err, ErrSessionBusy) {
		return err
	}
	owner, ok := parseLockOwner(f)
	if !ok || owner.PID <= 0 {
		return fmt.Errorf("session %q: %w (cannot determine lock owner to steal)", id, ErrSessionBusy)
	}
	if owner.PID == os.Getpid() {
		return fmt.Errorf("session %q: %w (already owned by current process pid=%d)", id, ErrSessionBusy, owner.PID)
	}
	localHost, _ := os.Hostname()
	if localHost != "" && owner.Host != "" && owner.Host != localHost {
		return fmt.Errorf("session %q: %w (owned by remote host %q, current host %q; refusing to signal pid=%d)", id, ErrSessionBusy, owner.Host, localHost, owner.PID)
	}
	if processAlive(owner.PID) {
		if localHost == "" || owner.Host == "" || owner.Heartbeat == "" || owner.ProcessStarted == "" {
			return fmt.Errorf("session %q: %w (cannot verify heartbeat and identity of owner pid=%d)", id, ErrSessionBusy, owner.PID)
		}
		if !lockOwnerStale(owner, time.Now()) {
			return fmt.Errorf("session %q: %w (owner pid=%d is still active; refusing to steal)", id, ErrSessionBusy, owner.PID)
		}
		if err := tryLockWriter(f); err == nil {
			return nil
		} else if !errors.Is(err, ErrSessionBusy) {
			return err
		}
		if latest, ok := parseLockOwner(f); !ok || latest != owner {
			return fmt.Errorf("session %q: %w (lock owner or heartbeat changed)", id, ErrSessionBusy)
		}
		if err := terminateWriterProcess(owner); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("session %q: %w (cannot terminate stalled owner pid=%d: %v)", id, ErrSessionBusy, owner.PID, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := tryLockWriter(f); err == nil {
			return nil
		} else if !errors.Is(err, ErrSessionBusy) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("session %q: %w (stalled owner pid=%d did not release lock in time)", id, ErrSessionBusy, owner.PID)
}

func newLockOwner() LockOwner {
	host, _ := os.Hostname()
	pid := os.Getpid()
	return LockOwner{
		PID: pid, Host: host, Started: time.Now().UTC().Format(time.RFC3339),
		ProcessStarted: writerProcessIdentity(pid),
	}
}

func writeLockOwner(f *os.File, owner LockOwner) error {
	owner.Heartbeat = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	return writeLockOwnerData(f, data)
}

func writeLockOwnerData(f *os.File, data []byte) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	// Reserve byte zero for the Windows writer lease so diagnostics can
	// read the record starting immediately after the locked byte.
	_, err := f.Write(append(append([]byte{'\n'}, data...), '\n'))
	return err
}

func parseLockOwner(f *os.File) (LockOwner, bool) {
	raw := readLockOwner(f)
	if raw == "" {
		return LockOwner{}, false
	}
	var owner LockOwner
	if json.Unmarshal([]byte(raw), &owner) == nil && owner.PID > 0 {
		return owner, true
	}
	// Legacy format: "pid=123 started=2026-..."
	var pid int
	var started string
	if _, err := fmt.Sscanf(raw, "pid=%d started=%s", &pid, &started); err == nil && pid > 0 {
		return LockOwner{PID: pid, Started: started}, true
	}
	return LockOwner{}, false
}

func lockOwnerStale(owner LockOwner, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, owner.Heartbeat)
	if err != nil {
		return false
	}
	return now.Sub(t) > lockStaleTTL
}

func formatLockOwner(f *os.File) string {
	owner, ok := parseLockOwner(f)
	if !ok {
		return readLockOwner(f)
	}
	status := "dead"
	if processAlive(owner.PID) {
		status = "alive"
		if lockOwnerStale(owner, time.Now()) {
			status = "stalled"
		}
	}
	parts := []string{fmt.Sprintf("pid=%d", owner.PID), "status=" + status}
	if owner.Started != "" {
		parts = append(parts, "started="+owner.Started)
	}
	if owner.Heartbeat != "" {
		parts = append(parts, "heartbeat="+owner.Heartbeat)
	}
	return strings.Join(parts, " ")
}

func readLockOwner(f *os.File) string {
	if _, err := f.Seek(lockOwnerReadOffset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, 512))
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
