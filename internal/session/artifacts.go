package session

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const maxSessionArtifacts = 8192
const maxArtifactScan = 100000

type Artifact struct {
	Path            string `json:"path"` // relative to the sessions directory
	Kind            string `json:"kind"`
	archiveRecovery bool
}

type ArtifactManifest struct {
	Version   int        `json:"version"`
	ID        string     `json:"id"`
	OutputID  string     `json:"output_id,omitempty"`
	Shared    bool       `json:"shared"`
	Artifacts []Artifact `json:"artifacts"`
	archives  []string   // Validated family archives, including shared files.
}

type artifactRegistry struct {
	Version  int       `json:"version"`
	Created  time.Time `json:"created"`
	Archives []string  `json:"archives"`
	Promoted []string  `json:"promoted,omitempty"`
}

func readArtifactHeader(path string) (*Header, error) {
	if err := checkTranscriptPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|metadataReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact must be a regular file")
	}
	h, _, err := readHeader(f)
	return h, err
}

func registryPath(h *Header) string { return filepath.Join(Dir(), h.OutputID, "artifacts.json") }

func loadArtifactRegistry(h *Header) (artifactRegistry, error) {
	r := artifactRegistry{Version: 1}
	if h == nil || h.OutputID == "" {
		return r, nil
	}
	r.Created = h.Created
	path := registryPath(h)
	if _, err := metadataDirectory(h.OutputID, false); err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return r, err
	}
	if err := checkTranscriptPath(path); err != nil {
		return r, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|metadataReadFlags, 0)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return r, err
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return r, fmt.Errorf("invalid artifact registry")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	return r, validateArtifactRegistry(r, h.Created)
}

func validateArtifactRegistry(r artifactRegistry, created time.Time) error {
	if r.Version != 1 || !r.Created.Equal(created) || len(r.Archives)+len(r.Promoted) > maxSessionArtifacts {
		return fmt.Errorf("artifact registry owner/version invalid")
	}
	names := make(map[string]bool, len(r.Archives)+len(r.Promoted))
	for _, entries := range [][]string{r.Archives, r.Promoted} {
		for _, name := range entries {
			if !strings.HasSuffix(name, ".jsonl") || validateSessionName(strings.TrimSuffix(name, ".jsonl"), 240) != nil || names[name] {
				return fmt.Errorf("invalid or duplicate archive registry path")
			}
			names[name] = true
		}
	}
	return nil
}

func saveArtifactRegistry(h *Header, r artifactRegistry) error {
	if h == nil || h.OutputID == "" {
		return fmt.Errorf("artifact registration requires a bound session")
	}
	if err := validateArtifactRegistry(r, h.Created); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("artifact registry exceeds limit")
	}
	return writeArtifactMetadata(h.OutputID, "artifacts.json", data)
}

func (s *Store) registerArchiveLocked(name string) error {
	r, err := loadArtifactRegistry(s.header)
	if err != nil {
		return err
	}
	for _, entry := range r.Archives {
		if entry == name {
			return nil
		}
	}
	r.Promoted = slices.DeleteFunc(r.Promoted, func(entry string) bool { return entry == name })
	r.Archives = append(r.Archives, name)
	sort.Strings(r.Archives)
	return saveArtifactRegistry(s.header, r)
}

// A writable resume turns an immutable archive into a session that still
// shares the artifact family. Record the promotion before accepting work.
func (s *Store) promoteArchive() error {
	r, err := loadArtifactRegistry(s.header)
	if err != nil {
		return err
	}
	name := s.ID + ".jsonl"
	if slices.Contains(r.Promoted, name) {
		return nil
	}
	if !slices.Contains(r.Archives, name) && !legacyArchiveName(s.ID) {
		return nil
	}
	r.Archives = slices.DeleteFunc(r.Archives, func(entry string) bool { return entry == name })
	r.Promoted = append(r.Promoted, name)
	sort.Strings(r.Promoted)
	return saveArtifactRegistry(s.header, r)
}

func legacyArchiveName(id string) bool {
	return strings.Contains(id, "-pre-compact-") || strings.Contains(id, "-pre-rollback-")
}

// Manifest identifies the entire owned family. Stable output identity keeps
// usage, captures and delivery receipts attached across transcript renames.
func (s *Store) Manifest() (ArtifactManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ArtifactManifest{}, os.ErrClosed
	}
	unlock, err := lockManagement()
	if err != nil {
		return ArtifactManifest{}, err
	}
	defer unlock()
	return sessionManifest(s.ID)
}

// Earlier bound archives may predate the output claim. Their creation time
// still has to match, and an explicit conflicting output ID is never adopted.
func matchesArchive(owner, archive *Header) bool {
	if archive == nil {
		return true
	}
	return owner != nil && owner.Created.Equal(archive.Created) &&
		(archive.OutputID == "" || owner.OutputID == archive.OutputID)
}

func sessionManifest(id string) (ArtifactManifest, error) {
	m := ArtifactManifest{Version: 1, ID: id}
	h, err := readArtifactHeader(filepath.Join(Dir(), id+".jsonl"))
	if err != nil {
		return m, err
	}
	if h != nil {
		m.OutputID = h.OutputID
	}
	r, err := loadArtifactRegistry(h)
	if err != nil {
		return m, err
	}
	entries, err := artifactEntries()
	if err != nil {
		return m, err
	}
	owned := map[string]string{id + ".jsonl": "transcript"}
	audit := id + "-audit.jsonl"
	if ah, err := readArtifactHeader(filepath.Join(Dir(), audit)); err == nil {
		// An independently bound transcript whose name ends in -audit is not
		// this owner's sidecar.
		if ah == nil {
			owned[audit] = "audit"
		}
	} else if !os.IsNotExist(err) {
		return m, err
	}

	for _, name := range r.Archives {
		ah, err := readArtifactHeader(filepath.Join(Dir(), name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return m, err
		}
		if !matchesArchive(h, ah) {
			continue
		}
		owned[name] = "archive"
	}

	// Adopt the established archive naming contract. Bound mismatches are other
	// sessions, even when their names happen to share an archive prefix.
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".jsonl") && !slices.Contains(r.Promoted, name) &&
			(strings.HasPrefix(name, id+"-pre-compact-") || strings.HasPrefix(name, id+"-pre-rollback-")) {
			ah, err := readArtifactHeader(filepath.Join(Dir(), name))
			if err != nil {
				return m, err
			}
			if !matchesArchive(h, ah) {
				continue
			}
			owned[name] = "archive"
		}
	}
	// A copied transcript still refers to its stable artifact family. Keep the
	// shared archives/output root until the final referencing session is deleted.
	if h != nil && h.OutputID != "" {
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") || owned[name] != "" {
				continue
			}
			other, err := readArtifactHeader(filepath.Join(Dir(), name))
			if err != nil {
				return m, err
			}
			if other != nil && other.OutputID == h.OutputID {
				m.Shared = true
				break
			}
		}
	}
	for name, kind := range owned {
		if kind == "archive" {
			m.archives = append(m.archives, name)
			if m.Shared {
				delete(owned, name)
			}
		}
	}
	sort.Strings(m.archives)
	archiveRecovery := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		marker := strings.LastIndex(name, ".jsonl-torn-")
		if marker < 0 || !strings.HasSuffix(name, ".bak") {
			continue
		}
		owner := name[:marker+len(".jsonl")]
		nonce := name[marker+len(".jsonl-torn-") : len(name)-4]
		ownerKind := owned[owner]
		if ownerKind == "" {
			for candidate, kind := range owned {
				if !strings.EqualFold(candidate, owner) {
					continue
				}
				a, err := os.Stat(filepath.Join(Dir(), owner))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return m, err
				}
				b, err := os.Stat(filepath.Join(Dir(), candidate))
				if err != nil {
					return m, err
				}
				if os.SameFile(a, b) {
					ownerKind = kind
					break
				}
			}
		}
		if ownerKind != "" && validBackupID(nonce) {
			owned[name] = "recovery"
			archiveRecovery[name] = ownerKind == "archive"
		}
	}
	if h != nil && h.OutputID != "" && !m.Shared {
		if st, err := os.Lstat(filepath.Join(Dir(), h.OutputID)); err == nil {
			if !st.IsDir() {
				return m, fmt.Errorf("session artifact family must be a directory without symlinks")
			}
			owned[h.OutputID] = "output_family"
		} else if !os.IsNotExist(err) {
			return m, err
		}
	}
	for path, kind := range owned {
		st, err := os.Lstat(filepath.Join(Dir(), path))
		if err != nil {
			return m, err
		}
		if kind != "output_family" && !st.Mode().IsRegular() {
			return m, fmt.Errorf("session artifact %q must be a regular file", path)
		}
		m.Artifacts = append(m.Artifacts, Artifact{Path: path, Kind: kind, archiveRecovery: archiveRecovery[path]})
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].Path < m.Artifacts[j].Path })
	if len(m.Artifacts) > maxSessionArtifacts {
		return m, fmt.Errorf("too many session artifacts")
	}
	return m, nil
}

func artifactEntries() ([]os.DirEntry, error) {
	f, err := os.Open(Dir())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []os.DirEntry
	for {
		batch, err := f.ReadDir(256)
		if len(entries)+len(batch) > maxArtifactScan {
			return nil, fmt.Errorf("session artifact scan exceeds limit")
		}
		entries = append(entries, batch...)
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Filesystem aliases select the existing basename so recovery backups and
// sidecars are found consistently on case-insensitive filesystems.
func canonicalSessionID(id string) (string, error) {
	st, err := os.Stat(filepath.Join(Dir(), id+".jsonl"))
	if os.IsNotExist(err) {
		return id, nil
	}
	if err != nil {
		return "", err
	}
	entries, err := artifactEntries()
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.EqualFold(name, id+".jsonl") {
			other, err := entry.Info()
			if err != nil {
				return "", err
			}
			if os.SameFile(st, other) {
				return strings.TrimSuffix(name, ".jsonl"), nil
			}
		}
	}
	return id, nil
}
