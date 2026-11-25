package session

import (
	"errors"
	"fmt"
	"minimalharness/internal/privatefile"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Info is one row of the session list: the id, how many messages a
// --resume would load, when it was last written, and the first user
// message as a preview.
type Info struct {
	ID       string
	Messages int
	Modified time.Time
	Preview  string
}

// maxListSessions caps the list: the directory can hold years of
// runs, and the list is for picking a recent one, not an archive.
const maxListSessions = 30

// List returns the sessions in Dir(), newest first. Audit sidecars
// and pre-compact archives are not sessions, so they never appear.
func List() ([]Info, error) {
	unlock, err := lockManagement()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := recoverOperations(); err != nil {
		return nil, err
	}

	dir := Dir()
	entries, err := artifactEntries()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Registries identify arbitrary archive names without relying on a suffix.
	headers := map[string]*Header{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonl") || entry.IsDir() {
			continue
		}
		h, err := readArtifactHeader(filepath.Join(dir, entry.Name()))
		if err == nil {
			headers[entry.Name()] = h
		}
	}
	archives := map[string]bool{}
	promoted := map[string]bool{}
	seen := map[string]bool{}
	for _, h := range headers {
		if h == nil || h.OutputID == "" || seen[h.OutputID] {
			continue
		}
		seen[h.OutputID] = true
		r, err := loadArtifactRegistry(h)
		if err != nil {
			return nil, err
		}
		for _, name := range r.Promoted {
			promoted[name] = true
		}
		for _, name := range r.Archives {
			ah, exists := headers[name]
			if exists && (matchesArchive(h, ah)) {
				archives[name] = true
			}
		}
	}
	var out []Info
	for _, en := range entries {
		name := en.Name()
		if en.IsDir() || !strings.HasSuffix(name, ".jsonl") || archives[name] {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if headers[name] == nil && strings.HasSuffix(id, "-audit") || !promoted[name] && legacyArchiveName(id) {
			continue
		}
		info, err := readInfo(filepath.Join(dir, name), id)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	if len(out) > maxListSessions {
		out = out[:maxListSessions]
	}
	return out, nil
}

// readInfo counts the messages a --resume would load (a /clear or
// /compact marker resets the count, like Load) and takes the first
// user message as the preview.
func readInfo(path, id string) (Info, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	info := Info{ID: id, Modified: st.ModTime()}
	msgs, err := Load(id)
	if err != nil {
		return Info{}, err
	}
	info.Messages = len(msgs)
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			info.Preview = truncateRunes(strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " "), 60)
			break
		}
	}
	return info, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ValidateSessionID rejects ids that cannot be a file name in the
// sessions directory: empty, dot names, separators, and anything
// outside letters, digits, "-" and "_". (Dots are allowed inside an
// id, just not as the whole id.)
func ValidateSessionID(id string) error { return validateSessionName(id, 128) }

func validateSessionName(id string, maxBytes int) error {
	if id == "" {
		return fmt.Errorf("session id is empty")
	}
	if id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid session id %q", id)
	}
	if len(id) > maxBytes {
		return fmt.Errorf("session id too long (max %d): %q", maxBytes, id)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid session id %q (letters, digits, -, _ and . only)", id)
	}
	return nil
}

// renameSessionFiles moves a session's transcript, its audit sidecar
// and its pre-compact archives to a new id. The sidecar and archives
// are optional: a missing one is not an error.
func renameSessionFiles(oldID, newID string) error {
	if err := ValidateSessionID(oldID); err != nil {
		return err
	}
	if err := ValidateSessionID(newID); err != nil {
		return err
	}
	if oldID == newID {
		return fmt.Errorf("session %q already has that name", oldID)
	}
	if _, err := os.Lstat(filepath.Join(Dir(), newID+".jsonl")); err == nil {
		return fmt.Errorf("session %q already exists", newID)
	} else if !os.IsNotExist(err) {
		return err
	}
	m, err := sessionManifest(oldID)
	if os.IsNotExist(err) {
		return fmt.Errorf("unknown session %q", oldID)
	}
	if err != nil {
		return err
	}
	op, err := prepareOperation("rename", oldID, newID, m)
	if err != nil {
		return err
	}
	return performOperation(op)
}

// RenameSession renames a session that is not currently open. For the
// open store, use Store.Rename, which also re-points the handles.
func RenameSession(oldID, newID string) error {
	if err := ValidateSessionID(oldID); err != nil {
		return err
	}
	if err := ValidateSessionID(newID); err != nil {
		return err
	}
	unlock, err := lockManagement()
	if err != nil {
		return err
	}
	defer unlock()
	oldID, err = canonicalSessionID(oldID)
	if err != nil {
		return err
	}
	recovered, err := recoverOperations(oldID, newID)
	if err != nil {
		return err
	}
	for _, op := range recovered {
		if op.kind == "rename" && op.from == oldID && op.to == newID {
			return nil
		}
	}
	old, err := acquireWriter(oldID)
	if err != nil {
		return err
	}
	defer old.Close()
	target, err := acquireWriter(newID)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := pinLegacyOutputID(oldID); err != nil {
		return err
	}
	h, err := readArtifactHeader(filepath.Join(Dir(), oldID+".jsonl"))
	if err != nil {
		return err
	}
	if h != nil && h.OutputID != "" {
		if err := recoverOutputOperations(h.OutputID); err != nil {
			return err
		}
		output, err := acquireOutputWriter(h, oldID)
		if err != nil {
			return err
		}
		defer output.Close()
	}
	return renameSessionFiles(oldID, newID)
}

// Offline rename pins old output ownership before changing the filename.
// Unbound legacy sessions with output must be adopted first.
func pinLegacyOutputID(id string) error {
	path := filepath.Join(Dir(), id+".jsonl")
	if err := checkTranscriptPath(path); err != nil {
		return err
	}
	f, err := privatefile.Open(path, os.O_RDWR|metadataReadFlags)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("unknown session %q", id)
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("session transcript must be a regular file")
	}
	if err := checkTranscriptLinks(f); err != nil {
		return err
	}
	h, skip, err := readHeader(f)
	if err != nil {
		return err
	}
	if h == nil {
		if _, err := os.Stat(filepath.Join(Dir(), id, "bash-output")); err == nil {
			return fmt.Errorf("adopt legacy session with --rebind-session before renaming its output")
		} else if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if h.OutputID != "" {
		return nil
	}
	output, err := acquireOutputWriter(h, id)
	if err != nil {
		return err
	}
	defer output.Close()
	s := &Store{ID: id, Path: path, file: f}
	defer func() { _ = s.file.Close() }()
	return s.replaceHeader(h, skip)
}

// DeleteSession removes a session that is not currently open: its
// transcript, its audit sidecar and its pre-compact archives. The
// sidecar and archives are optional; a missing one is not an error.
// The open store is never deleted through this — delete another
// session, or resume a different one first.
func DeleteSession(id string) error {
	if err := ValidateSessionID(id); err != nil {
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
	recovered, err := recoverOperations(id)
	if err != nil {
		return err
	}
	for _, op := range recovered {
		if op.kind == "delete" && op.from == id {
			return nil
		}
	}
	writer, err := acquireWriter(id)
	if err != nil {
		return err
	}
	defer writer.Close()
	if err := pinLegacyOutputID(id); err != nil {
		return err
	}
	h, err := readArtifactHeader(filepath.Join(Dir(), id+".jsonl"))
	if os.IsNotExist(err) {
		return fmt.Errorf("unknown session %q", id)
	}
	if err != nil {
		return err
	}
	if h != nil && h.OutputID != "" {
		if err := recoverOutputOperations(h.OutputID); err != nil {
			return err
		}
		output, err := acquireOutputWriter(h, id)
		if err != nil {
			return err
		}
		defer output.Close()
	}
	m, err := sessionManifest(id)
	if err != nil {
		return err
	}
	op, err := prepareOperation("delete", id, "", m)
	if err != nil {
		return err
	}
	if err := removeReplayBesideSession(id); err != nil {
		return errors.Join(err, cleanupOperation(op))
	}
	return performOperation(op)
}

func replayRoot() (*os.Root, error) {
	parent, err := os.OpenRoot(Dir())
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	observed, err := parent.Lstat("rpc-replay")
	if err != nil {
		return nil, err
	}
	if observed.Mode()&os.ModeSymlink != 0 || !observed.IsDir() {
		return nil, fmt.Errorf("rpc replay directory must not be an alias")
	}
	child, err := parent.OpenRoot("rpc-replay")
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(observed, opened) {
		child.Close()
		return nil, fmt.Errorf("rpc replay directory changed while opening")
	}
	return child, nil
}

func replayFileInfo(id string) (os.FileInfo, error) {
	if err := ValidateSessionID(id); err != nil {
		return nil, err
	}
	root, err := replayRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Lstat(id + ".jsonl")
}

func reconcileReplay(op artifactOperation, forward bool) error {
	if !op.Replay || op.Kind != "rename" {
		return nil
	}
	from, to := op.OldID+".jsonl", op.NewID+".jsonl"
	if !forward {
		from, to = to, from
	}
	root, err := replayRoot()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close()
	_, srcErr := root.Lstat(from)
	_, dstErr := root.Lstat(to)
	if os.IsNotExist(srcErr) && (dstErr == nil || os.IsNotExist(dstErr)) {
		return nil
	}
	if srcErr != nil {
		return srcErr
	}
	if dstErr == nil {
		if !forward {
			return nil
		}
		return fmt.Errorf("replay log %s already exists", to)
	}
	if !os.IsNotExist(dstErr) {
		return dstErr
	}
	return root.Rename(from, to)
}

func removeReplayBesideSession(id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	root, err := replayRoot()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close()
	err = root.Remove(id + ".jsonl")
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Rename renames the open store's session: the handles are closed,
// the files are moved (transcript, audit sidecar, archives), and the
// transcript is reopened at the new path. The store stays usable
// afterwards; the runner's totals are the caller's to reset.
func (s *Store) Rename(newID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	unlock, err := lockManagement()
	if err != nil {
		return err
	}
	defer unlock()
	oldID := s.ID
	// Validate/existence checks before taking the destination lease also avoid
	// treating a case-only rename of the active file as a competing writer.
	if err := ValidateSessionID(newID); err != nil {
		return err
	}
	if _, err := recoverOperations(oldID, newID); err != nil {
		return fmt.Errorf("resolve outstanding rename before changing names: %w", err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), newID+".jsonl")); err == nil {
		return fmt.Errorf("session %q already exists", newID)
	} else if !os.IsNotExist(err) {
		return err
	}
	target, err := acquireWriter(newID)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			target.Close()
		}
	}()
	if s.auditFile != nil {
		s.auditFile.Close()
		s.auditFile = nil
	}
	s.file.Close()
	if err := renameSessionFiles(oldID, newID); err != nil {
		// A failed rollback freezes writes until the journal can be recovered.
		if errors.Is(err, ErrOperationPending) {
			s.file = nil
			return err
		}
		f, err2 := privatefile.Open(s.Path, os.O_APPEND|os.O_RDWR)
		if err2 == nil {
			s.file = f
		}
		return err
	}
	// The rename has committed: retain the new lease even if reopening fails.
	if s.stopHeartbeat != nil {
		s.stopHeartbeat()
		s.stopHeartbeat = nil
	}
	s.writer.Close()
	s.writer = target
	s.stopHeartbeat = startWriterHeartbeat(target)
	keep = true
	newPath := filepath.Join(Dir(), newID+".jsonl")
	s.ID, s.Path = newID, newPath
	f, err := privatefile.Open(newPath, os.O_APPEND|os.O_RDWR)
	if err != nil {
		return err
	}
	s.file = f
	s.ID = newID
	s.Path = newPath
	return nil
}
