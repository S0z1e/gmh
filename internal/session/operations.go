package session

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrOperationPending = errors.New("session operation needs recovery; close its writer and retry")

type artifactMove struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}
type artifactOperation struct {
	Version int              `json:"version"`
	Nonce   string           `json:"nonce"`
	Kind    string           `json:"kind"`
	OldID   string           `json:"old_id"`
	NewID   string           `json:"new_id,omitempty"`
	Header  *Header          `json:"header,omitempty"`
	Moves   []artifactMove   `json:"moves"`
	Before  artifactRegistry `json:"before"`
	After   artifactRegistry `json:"after"`
}

// The management lock protects plans and namespace changes, not active turns.
// Lifetime writer/output leases still protect each family's content and jobs.
func lockManagement() (func(), error) {
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		return nil, err
	}
	dir, err := metadataDirectory(".management", true)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "namespace.lock")
	if err := checkTranscriptPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|metadataReadFlags, 0600)
	if err != nil {
		return nil, err
	}
	if err := checkTranscriptLinks(f); err != nil {
		f.Close()
		return nil, err
	}
	unlock, err := lockTranscript(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() { unlock(); f.Close() }, nil
}

func operationPath(op artifactOperation) string {
	return filepath.Join(Dir(), ".operations", op.Nonce+".json")
}
func operationData(op artifactOperation) string {
	return filepath.Join(".operations", op.Nonce+".data")
}

func prepareOperation(kind, oldID, newID string, m ArtifactManifest) (artifactOperation, error) {
	op := artifactOperation{Version: 1, Nonce: rand.Text(), Kind: kind, OldID: oldID, NewID: newID}
	var err error
	op.Header, err = readArtifactHeader(filepath.Join(Dir(), oldID+".jsonl"))
	if err != nil {
		return op, err
	}
	op.Before, err = loadArtifactRegistry(op.Header)
	if err != nil {
		return op, err
	}
	op.After = op.Before
	op.After.Archives = append([]string(nil), op.Before.Archives...)
	rename := func(name string) string {
		if len(name) > len(oldID) && strings.EqualFold(name[:len(oldID)], oldID) && strings.HasPrefix(name[len(oldID):], ".jsonl-torn-") {
			return newID + name[len(oldID):]
		}
		if len(name) > len(oldID) && strings.EqualFold(name[:len(oldID)], oldID) && strings.HasPrefix(name[len(oldID):], "-") {
			return newID + name[len(oldID):]
		}
		return name
	}
	for i, name := range op.After.Archives {
		op.After.Archives[i] = rename(name)
	}
	if m.Shared {
		op.After = op.Before
	}
	var transcript artifactMove
	for _, a := range m.Artifacts {
		if kind == "rename" && a.Kind == "output_family" {
			continue
		}
		to := rename(a.Path)
		if kind == "delete" {
			to = filepath.Join(operationData(op), a.Path)
		}
		if a.Kind == "transcript" {
			if kind == "rename" {
				to = newID + ".jsonl"
			}
			transcript = artifactMove{a.Path, to, a.Kind}
			continue
		}
		if a.Path == to {
			continue
		}
		op.Moves = append(op.Moves, artifactMove{a.Path, to, a.Kind})
	}
	op.Moves = append(op.Moves, transcript)
	if err := validateOperation(op); err != nil {
		return op, err
	}
	for _, move := range op.Moves {
		if _, err := os.Lstat(filepath.Join(Dir(), move.To)); err == nil {
			return op, fmt.Errorf("artifact destination %q already exists", move.To)
		} else if !os.IsNotExist(err) {
			return op, err
		}
	}
	dir, err := metadataDirectory(".operations", true)
	if err != nil {
		return op, err
	}
	if kind == "delete" {
		root, err := os.OpenRoot(dir)
		if err != nil {
			return op, err
		}
		err = root.Mkdir(op.Nonce+".data", 0700)
		root.Close()
		if err != nil {
			return op, err
		}
		if err := syncRecoveryDirectory(dir); err != nil {
			return op, err
		}
	}

	data, err := json.Marshal(op)
	if err != nil {
		return op, err
	}
	if len(data) > 4<<20 {
		return op, fmt.Errorf("operation manifest exceeds limit")
	}
	if err := writeArtifactMetadata(".operations", op.Nonce+".json", data); err != nil {
		return op, err
	}

	return op, nil
}

func validateOperation(op artifactOperation) error {
	if op.Version != 1 || !validBackupID(op.Nonce) || (op.Kind != "rename" && op.Kind != "delete") || ValidateSessionID(op.OldID) != nil || len(op.Moves) == 0 || len(op.Moves) > maxSessionArtifacts {
		return fmt.Errorf("invalid session operation manifest")
	}
	if op.Kind == "rename" && (ValidateSessionID(op.NewID) != nil || op.NewID == op.OldID) {
		return fmt.Errorf("invalid rename manifest")
	}
	if op.Header != nil {
		data, err := json.Marshal(op.Header)
		if err != nil {
			return err
		}
		if _, err := decodeHeader(data); err != nil {
			return err
		}
	}
	archives := map[string]bool{}
	for _, r := range []artifactRegistry{op.Before, op.After} {
		if r.Version != 1 || len(r.Archives) > maxSessionArtifacts || op.Header != nil && !r.Created.Equal(op.Header.Created) {
			return fmt.Errorf("invalid operation archive owner")
		}
		for _, name := range r.Archives {
			if !strings.HasSuffix(name, ".jsonl") || validateSessionName(strings.TrimSuffix(name, ".jsonl"), 240) != nil {
				return fmt.Errorf("invalid operation archive name")
			}
		}
	}
	for _, name := range op.Before.Archives {
		archives[name] = true
	}
	sources, targets := map[string]bool{}, map[string]bool{}
	for i, m := range op.Moves {
		if filepath.Base(m.From) != m.From || validateSessionName(m.From, 255) != nil || sources[m.From] || targets[m.To] {
			return fmt.Errorf("invalid/duplicate artifact move")
		}
		sources[m.From] = true
		targets[m.To] = true
		switch m.Kind {
		case "transcript":
			if i != len(op.Moves)-1 || m.From != op.OldID+".jsonl" {
				return fmt.Errorf("transcript must commit last")
			}
		case "audit":
			if m.From != op.OldID+"-audit.jsonl" {
				return fmt.Errorf("invalid audit owner")
			}
		case "archive":
			if !archives[m.From] && !(strings.HasSuffix(m.From, ".jsonl") && (strings.HasPrefix(m.From, op.OldID+"-pre-compact-") || strings.HasPrefix(m.From, op.OldID+"-pre-rollback-"))) {
				return fmt.Errorf("invalid archive owner")
			}
		case "recovery":
			marker := strings.LastIndex(m.From, ".jsonl-torn-")
			if marker < 0 || !strings.HasSuffix(m.From, ".bak") || !validBackupID(m.From[marker+len(".jsonl-torn-"):len(m.From)-4]) {
				return fmt.Errorf("invalid recovery name")
			}
			owner := m.From[:marker+len(".jsonl")]
			if !strings.EqualFold(owner, op.OldID+".jsonl") && !archives[owner] && !(strings.HasPrefix(owner, op.OldID+"-pre-compact-") || strings.HasPrefix(owner, op.OldID+"-pre-rollback-")) {
				return fmt.Errorf("invalid recovery owner")
			}
		case "output_family":
			if op.Kind != "delete" || op.Header == nil || m.From != op.Header.OutputID {
				return fmt.Errorf("invalid output owner")
			}
		default:
			return fmt.Errorf("invalid artifact kind")
		}
		if op.Kind == "delete" {
			if m.To != filepath.Join(operationData(op), m.From) {
				return fmt.Errorf("invalid quarantine destination")
			}
		} else {
			expected := op.NewID + ".jsonl"
			if m.Kind != "transcript" {
				if len(m.From) <= len(op.OldID) || !strings.EqualFold(m.From[:len(op.OldID)], op.OldID) {
					return fmt.Errorf("invalid rename owner")
				}
				expected = op.NewID + m.From[len(op.OldID):]
			}
			if m.To != expected || validateSessionName(m.To, 255) != nil {
				return fmt.Errorf("invalid rename destination")
			}
		}
	}
	if op.Moves[len(op.Moves)-1].Kind != "transcript" {
		return fmt.Errorf("transcript must commit last")
	}
	return nil
}

func reconcileMove(m artifactMove, forward bool) error {
	from, to := m.From, m.To
	if !forward {
		from, to = to, from
	}
	root, err := os.OpenRoot(Dir())
	if err != nil {
		return err
	}
	defer root.Close()
	_, srcErr := root.Lstat(from)
	_, dstErr := root.Lstat(to)
	if os.IsNotExist(srcErr) && dstErr == nil {
		return nil
	}
	if srcErr != nil {
		return srcErr
	}
	if dstErr == nil {
		return fmt.Errorf("both source and destination exist for %q", m.From)
	}
	if !os.IsNotExist(dstErr) {
		return dstErr
	}
	if err := root.Rename(from, to); err != nil {
		return err
	}
	// Persist both directory entries before the transcript commit point.
	if err := syncRecoveryDirectory(filepath.Join(Dir(), filepath.Dir(to))); err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Join(Dir(), filepath.Dir(from)))
}

func reconcileOperation(op artifactOperation, forward bool) error {
	var result error
	if forward {
		for _, move := range op.Moves {
			if err := reconcileMove(move, true); err != nil {
				return err
			}
		}
	} else {
		for i := len(op.Moves) - 1; i >= 0; i-- {
			result = errors.Join(result, reconcileMove(op.Moves[i], false))
		}
	}
	if result != nil {
		return result
	}
	if op.Kind == "rename" && op.Header != nil && op.Header.OutputID != "" {
		r := op.Before
		if forward {
			r = op.After
		}
		if err := saveArtifactRegistry(op.Header, r); err != nil {
			return err
		}
	}
	return syncRecoveryDirectory(Dir())
}

func cleanupOperation(op artifactOperation) error {
	root, err := os.OpenRoot(Dir())
	if err != nil {
		return err
	}
	defer root.Close()
	if op.Kind == "delete" {
		if err := root.RemoveAll(operationData(op)); err != nil {
			return err
		}
	}
	if err := root.Remove(filepath.Join(".operations", op.Nonce+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncRecoveryDirectory(filepath.Join(Dir(), ".operations"))
}

func performOperation(op artifactOperation) error {
	if err := reconcileOperation(op, true); err != nil {
		if rollbackErr := reconcileOperation(op, false); rollbackErr != nil {
			return errors.Join(err, rollbackErr, ErrOperationPending)
		}
		return errors.Join(err, cleanupOperation(op))
	}
	if op.Kind == "delete" {
		if err := cleanupOperation(op); err != nil {
			return errors.Join(err, ErrOperationPending)
		}
	} else {
		// A completed rename is already synced. A leftover committed plan is safe
		// to clean on the next open; it must not misreport a completed rename.
		_ = cleanupOperation(op)
	}
	return nil
}

func readOperation(path string) (artifactOperation, error) {
	var op artifactOperation
	if err := checkTranscriptPath(path); err != nil {
		return op, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|metadataReadFlags, 0)
	if err != nil {
		return op, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return op, err
	}
	if !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return op, fmt.Errorf("invalid operation metadata")
	}
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return op, err
	}
	if err := json.Unmarshal(data, &op); err != nil {
		return op, err
	}
	if err := validateOperation(op); err != nil {
		return op, err
	}
	if filepath.Base(path) != op.Nonce+".json" {
		return op, fmt.Errorf("operation filename mismatch")
	}
	return op, nil
}

// Recovery holds the management lock and acquires the affected lifetime leases.
// A busy live writer is left untouched; recovery never steals its session.
func recoverOperations(ids ...string) (bool, error) {
	dir := filepath.Join(Dir(), ".operations")
	if _, err := metadataDirectory(".operations", false); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maxSessionArtifacts + 1)
	if err == io.EOF {
		err = nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(entries) > maxSessionArtifacts {
		return false, fmt.Errorf("too many pending operations")
	}
	completed := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		op, err := readOperation(filepath.Join(dir, entry.Name()))
		if err != nil {
			return completed, err
		}
		relevant := len(ids) == 0
		for _, id := range ids {
			relevant = relevant || strings.EqualFold(id, op.OldID) || strings.EqualFold(id, op.NewID)
		}
		if !relevant {
			continue
		}
		old, err := acquireWriter(op.OldID)
		if err != nil {
			if len(ids) == 0 && errors.Is(err, ErrSessionBusy) {
				continue
			}
			return completed, err
		}
		var target *os.File
		if op.Kind == "rename" {
			target, err = acquireWriter(op.NewID)
		}
		if err != nil {
			old.Close()
			if len(ids) == 0 && errors.Is(err, ErrSessionBusy) {
				continue
			}
			return completed, err
		}
		var output *os.File
		if op.Header != nil && op.Header.OutputID != "" {
			h := *op.Header
			output, err = acquireOutputWriter(&h, op.OldID)
		}
		if err == nil {
			_, oldErr := os.Lstat(filepath.Join(Dir(), op.OldID+".jsonl"))
			forward := os.IsNotExist(oldErr)
			if oldErr != nil && !os.IsNotExist(oldErr) {
				err = oldErr
			} else if op.Kind == "delete" && forward {
				err = cleanupOperation(op)
			} else {
				err = reconcileOperation(op, forward)
				if err == nil {
					err = cleanupOperation(op)
				}
			}
			if err == nil {
				completed = true
			}
		}
		if output != nil {
			output.Close()
		}
		if target != nil {
			target.Close()
		}
		old.Close()
		if err != nil {
			if len(ids) == 0 && errors.Is(err, ErrSessionBusy) {
				continue
			}
			return completed, errors.Join(err, ErrOperationPending)
		}
	}
	return completed, nil
}
