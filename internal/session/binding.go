package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minimalharness/internal/privatefile"
	"minimalharness/internal/workspace"
)

const headerRole = "__session__"
const headerVersion = 1
const maxHeaderBytes = 64 * 1024

// Header is the first transcript record. It survives clear/compact/rollback and
// never becomes provider conversation content. Repository is optional metadata
// discovered without running git or loading repository configuration.
type Header struct {
	Role       string              `json:"role"`
	Version    int                 `json:"version"`
	Workspace  workspace.Identity  `json:"workspace"`
	Repository *workspace.Identity `json:"repository,omitempty"`
	Created    time.Time           `json:"created"`
	// OutputID remains stable across rename. New sessions never reuse a prior
	// session's output directory even when its transcript name becomes free.
	OutputID string `json:"output_id,omitempty"`
}

type OpenOptions struct {
	Workspace string
	// Existing requires a transcript after interrupted operations are recovered.
	Existing bool
	// Rebind is a direct user choice to adopt legacy history or move a session to
	// this workspace. Never set it from project config or an automatic resume.
	Rebind bool
}

// New opens a session bound to the current working directory. Frontends with an
// explicit workspace use Open instead. Legacy transcripts require explicit Rebind.
func New(id string) (*Store, error) { return Open(id, OpenOptions{}) }

func Open(id string, opts OpenOptions) (*Store, error) {
	return open(id, opts, (*Store).replaceHeader)
}

func open(id string, opts OpenOptions, replaceHeader func(*Store, *Header, int64) error) (*Store, error) {
	if id != "" {
		if err := ValidateSessionID(id); err != nil {
			return nil, err
		}
	}
	unlock, err := lockManagement()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if opts.Existing && id == "" {
		return nil, fmt.Errorf("an existing session id is required")
	}
	if id == "" {
		_, err = recoverOperations()
	} else {
		_, err = recoverOperations(id)
	}
	if err != nil {
		return nil, fmt.Errorf("recover session operations: %w", err)
	}
	if id != "" {
		id, err = canonicalSessionID(id)
		if err != nil {
			return nil, err
		}
	}
	if id != "" {
		header, err := readArtifactHeader(filepath.Join(Dir(), id+".jsonl"))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if header != nil {
			if err := recoverOutputOperations(header.OutputID); err != nil {
				return nil, fmt.Errorf("recover artifact family: %w", err)
			}
		}
	}
	identity, err := workspace.Identify(opts.Workspace)
	if err != nil {
		return nil, err
	}
	repo := repositoryIdentity(identity.Path)
	var header *Header
	var skip int64
	var replace bool
	var fresh bool
	s, err := newStore(id, opts.Existing, func(f *os.File, created bool) error {
		if created {
			fresh = true
			replace = true
			return nil
		}
		var err error
		header, skip, err = readHeader(f)
		if err != nil {
			return err
		}
		if !opts.Rebind {
			if err := checkBinding(header, identity, repo); err != nil {
				return fmt.Errorf("session %q: %w", id, err)
			}
		} else {
			replace = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	headerless := header == nil
	if replace {
		created := time.Now().UTC()
		outputID := ""
		if header != nil {
			created = header.Created
			outputID = header.OutputID
		}
		header = &Header{Role: headerRole, Version: headerVersion, Workspace: identity, Repository: repo, Created: created, OutputID: outputID}
	}
	if headerless && !fresh {
		// First adopt the headerless transcript durably. A failed/crashed
		// output claim must retry with this same creation identity.
		if err := replaceHeader(s, header, skip); err != nil {
			s.Close()
			return nil, err
		}
		encoded, err := json.Marshal(header)
		if err != nil {
			s.Close()
			return nil, err
		}
		skip = int64(len(encoded) + 1)
	}
	if header.OutputID == "" {
		replace = true
		if fresh {
			header.OutputID, err = randomOutputID()
			if err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	// A copied transcript must not acquire independent writers into one output
	// directory. This lease survives transcript rename and header replacement.
	s.outputWriter, err = acquireOutputWriter(header, s.ID)
	if err != nil {
		s.Close()
		return nil, err
	}
	if replace {
		if err := replaceHeader(s, header, skip); err != nil {
			s.Close()
			return nil, err
		}
	}
	s.header = header
	if _, err := s.UsageTotals(); err != nil {
		s.Close()
		return nil, fmt.Errorf("load session usage: %w", err)
	}
	if !fresh {
		if err := s.promoteArchive(); err != nil {
			s.Close()
			return nil, fmt.Errorf("promote resumed archive: %w", err)
		}
	}
	return s, nil
}

func checkBinding(h *Header, current workspace.Identity, repo *workspace.Identity) error {
	if h == nil {
		return fmt.Errorf("legacy session has no workspace binding; use --resume <id> --rebind-session to adopt it explicitly")
	}
	sameRepo := h.Repository == nil && repo == nil
	if h.Repository != nil && repo != nil {
		sameRepo = h.Repository.Matches(*repo)
	}
	if !h.Workspace.Matches(current) || !sameRepo {
		return fmt.Errorf("session workspace/repository differs from %q; use its original --root or --resume <id> --rebind-session to move it explicitly", h.Workspace.Path)
	}
	return nil
}

// CheckWorkspace validates an already owned Store before same-session reload.
func (s *Store) CheckWorkspace(root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	id, err := workspace.Identify(root)
	if err != nil {
		return err
	}
	return checkBinding(s.header, id, repositoryIdentity(id.Path))
}

func decodeHeader(line []byte) (*Header, error) {
	if len(line) > maxHeaderBytes {
		return nil, fmt.Errorf("session header exceeds %d bytes", maxHeaderBytes)
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, fmt.Errorf("invalid session header: %w", err)
	}
	if h.Role != headerRole || h.Version != headerVersion {
		return nil, fmt.Errorf("unsupported session header version %d", h.Version)
	}
	if h.Workspace.Path == "" || !absoluteIdentityPath(h.Workspace.Path) || h.Created.IsZero() {
		return nil, fmt.Errorf("session header is missing workspace or creation time")
	}
	if h.Repository != nil && (h.Repository.Path == "" || !absoluteIdentityPath(h.Repository.Path)) {
		return nil, fmt.Errorf("invalid session repository identity")
	}
	if h.OutputID != "" && ValidateSessionID(h.OutputID) != nil {
		return nil, fmt.Errorf("invalid session output identity")
	}
	return &h, nil
}

// Read only the first nonblank record, bounded by the transcript record limit.
// Legacy corruption is left for recoverTail after explicit adoption; default
// opens never repair or otherwise modify a differently bound/legacy transcript.
func readHeader(f *os.File) (*Header, int64, error) {
	reader := bufio.NewReaderSize(f, maxRecordBytes)
	var offset int64
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil && err != io.EOF {
			return nil, 0, err
		}
		offset += int64(len(line))
		if len(bytes.TrimSpace(line)) == 0 {
			if err == io.EOF {
				return nil, 0, nil
			}
			continue
		}
		var marker struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(line, &marker) != nil || marker.Role != headerRole {
			return nil, 0, nil
		}
		h, err := decodeHeader(line)
		return h, offset, err
	}
}

// Atomically prepend/replace metadata without replaying or discarding legacy
// records. The lifetime writer lease remains held across the inode replacement.
func (s *Store) replaceHeader(h *Header, skip int64) error {
	source, err := os.Open(s.Path)
	if err != nil {
		return err
	}
	defer source.Close()
	if _, err := source.Seek(skip, io.SeekStart); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".session-binding-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := json.NewEncoder(tmp).Encode(h); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, source); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Close every source handle before replacement (required on Windows).
	if err := source.Close(); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		s.file, _ = privatefile.Open(s.Path, os.O_APPEND|os.O_RDWR)
		return err
	}
	s.file, err = privatefile.Open(s.Path, os.O_APPEND|os.O_RDWR)
	if err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(s.Path))
}

func repositoryIdentity(root string) *workspace.Identity {
	for dir := root; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".git")
		info, err := os.Stat(marker)
		if err == nil {
			target := marker
			if info.Mode().IsRegular() {
				f, err := os.OpenFile(marker, os.O_RDONLY|metadataReadFlags, 0)
				if err != nil {
					return nil
				}
				st, err := f.Stat()
				if err != nil || !st.Mode().IsRegular() {
					f.Close()
					return nil
				}
				data, err := io.ReadAll(io.LimitReader(f, maxHeaderBytes+1))
				f.Close()
				if err != nil || len(data) > maxHeaderBytes {
					return nil
				}
				// Git removes trailing CR/LF, but spaces are part of the path.
				text := strings.TrimRight(string(data), "\r\n")
				if !strings.HasPrefix(text, "gitdir: ") {
					return nil
				}
				target = strings.TrimPrefix(text, "gitdir: ")
				if target == "" {
					return nil
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
			} else if !info.IsDir() {
				return nil
			}
			id, err := workspace.Identify(target)
			if err != nil || id.FileID == "" {
				return nil
			}
			return &id
		}
		if !os.IsNotExist(err) {
			return nil
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

// A copied session may use another OS's absolute path spelling. It can only be
// resumed here through an explicit rebind, but its schema remains readable.
func absoluteIdentityPath(path string) bool {
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return true
	}
	if len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '/' || path[2] == '\\') {
		return true
	}
	return strings.HasPrefix(path, `\\`)
}
