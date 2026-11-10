package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"minimalharness/internal/llm"
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
	dir := Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, en := range entries {
		name := en.Name()
		if en.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if strings.HasSuffix(id, "-audit") || strings.Contains(id, "-pre-compact-") {
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
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m llm.Message
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if m.Role == clearMarkerRole || m.Role == compactMarkerRole {
			info.Messages = 0
			info.Preview = ""
			continue
		}
		info.Messages++
		if info.Preview == "" && m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			one := strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " ")
			info.Preview = truncateRunes(one, 60)
		}
	}
	return info, sc.Err()
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
func ValidateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("session id is empty")
	}
	if id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid session id %q", id)
	}
	if len(id) > 128 {
		return fmt.Errorf("session id too long (max 128): %q", id)
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
	if err := ValidateSessionID(newID); err != nil {
		return err
	}
	if oldID == newID {
		return fmt.Errorf("session %q already has that name", oldID)
	}
	dir := Dir()
	oldPath := filepath.Join(dir, oldID+".jsonl")
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return fmt.Errorf("unknown session %q", oldID)
	}
	newPath := filepath.Join(dir, newID+".jsonl")
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("session %q already exists", newID)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	// Audit sidecar follows the transcript.
	_ = os.Rename(filepath.Join(dir, oldID+"-audit.jsonl"), filepath.Join(dir, newID+"-audit.jsonl"))
	// Pre-compact archives keep the family together.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, en := range entries {
			name := en.Name()
			if strings.HasPrefix(name, oldID+"-pre-compact-") && strings.HasSuffix(name, ".jsonl") {
				_ = os.Rename(filepath.Join(dir, name), filepath.Join(dir, newID+name[len(oldID):]))
			}
		}
	}
	return nil
}

// RenameSession renames a session that is not currently open. For the
// open store, use Store.Rename, which also re-points the handles.
func RenameSession(oldID, newID string) error {
	return renameSessionFiles(oldID, newID)
}

// Rename renames the open store's session: the handles are closed,
// the files are moved (transcript, audit sidecar, archives), and the
// transcript is reopened at the new path. The store stays usable
// afterwards; the runner's totals are the caller's to reset.
func (s *Store) Rename(newID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldID := s.ID
	if s.auditFile != nil {
		s.auditFile.Close()
		s.auditFile = nil
	}
	s.file.Close()
	if err := renameSessionFiles(oldID, newID); err != nil {
		// Put the store back on its old file so it stays usable.
		f, err2 := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err2 == nil {
			s.file = f
		}
		return err
	}
	newPath := filepath.Join(Dir(), newID+".jsonl")
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.file = f
	s.ID = newID
	s.Path = newPath
	return nil
}
