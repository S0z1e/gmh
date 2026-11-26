// Package session appends one JSON line per message to a session file
// and reloads it for --resume, repairing interrupted tool batches.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"minimalharness/internal/privatefile"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/llm"
)

type Store struct {
	ID            string
	Path          string
	file          *os.File
	writer        *os.File // lifetime lease, independent of rewritten transcript inode
	stopHeartbeat func()
	outputWriter  *os.File // output identity lease, independent of session name
	header        *Header
	auditFile     *os.File // lazily opened sidecar for audit records
	// mu guards Append and Audit: a background job's completion audit
	// can land on its own goroutine while the loop appends or audits
	// on the main one.
	mu               sync.Mutex
	closed           bool
	usageWriteFailed bool
	usageLoaded      bool
	usageTotals      UsageTotals
	usageInfo        os.FileInfo
	usageRecovery    UsageReconciliation
	usageChainSeq    int64
	usageChainDigest string
	usageChainOn     bool
}

// AuditPath is the sidecar file holding approval/bash audit records
// (one JSON line each), next to the session transcript.
func (s *Store) AuditPath() string {
	ext := filepath.Ext(s.Path)
	return s.Path[:len(s.Path)-len(ext)] + "-audit.jsonl"
}

// Audit appends one record to the audit sidecar. It never breaks the
// caller: errors are returned but the loop ignores them.
func (s *Store) Audit(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auditLocked(v)
}

func (s *Store) auditLocked(v any) error {
	unlock, err := lockManagement()
	if err != nil {
		return err
	}
	defer unlock()

	if !s.closed && s.file == nil {
		return ErrOperationPending
	}
	if s.closed {
		release, err := s.lockClosedStore()
		if err != nil {
			return err
		}
		defer release()

		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		f, err := openAuditSidecar(s.AuditPath())
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(b, '\n'))
		return err
	}
	if s.auditFile == nil {
		f, err := openAuditSidecar(s.AuditPath())
		if err != nil {
			return err
		}
		s.auditFile = f
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.auditFile.Write(append(b, '\n'))
	return err
}

// A closed store can service a late audit or archive request while its
// original owner is still present. The caller holds the management lock.
func (s *Store) lockClosedStore() (func(), error) {
	if _, err := recoverOperations(s.ID); err != nil {
		return nil, err
	}
	if s.header != nil {
		if err := recoverOutputOperations(s.header.OutputID); err != nil {
			return nil, err
		}
	}
	writer, err := acquireWriter(s.ID)
	if err != nil {
		return nil, err
	}
	header, err := readArtifactHeader(s.Path)
	if err != nil {
		writer.Close()
		return nil, err
	}
	if (s.header == nil) != (header == nil) || header != nil &&
		(!header.Created.Equal(s.header.Created) || header.OutputID != s.header.OutputID) {
		writer.Close()
		return nil, fmt.Errorf("session identity changed")
	}
	var output *os.File
	if header != nil && header.OutputID != "" {
		output, err = acquireOutputWriter(header, s.ID)
		if err != nil {
			writer.Close()
			return nil, err
		}
	}
	return func() {
		if output != nil {
			output.Close()
		}
		writer.Close()
	}, nil
}

func openAuditSidecar(path string) (*os.File, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if _, err := recoverOperations(id); err != nil {
		return nil, err
	}
	header, err := readArtifactHeader(path)
	if err == nil && header != nil {
		return nil, fmt.Errorf("audit path belongs to another session")
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := privatefile.Open(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|metadataReadFlags)
	if err != nil {
		return nil, err
	}
	if err := checkTranscriptLinks(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".minimal-harness", "sessions")
}

// createAutoStore runs under the management lock. A generated name is
// available only if both the transcript and prior operation reservations are clear.
func createAutoStore(dir, base string, preflight func(*os.File, bool) error) (*Store, error) {
	for suffix := 0; ; suffix++ {
		id := base
		if suffix > 0 {
			id = fmt.Sprintf("%s-%d", base, suffix)
		}
		if _, err := recoverOperations(id); err != nil {
			if errors.Is(err, ErrSessionBusy) {
				continue
			}
			return nil, err
		}
		writer, err := acquireWriter(id)
		if errors.Is(err, ErrSessionBusy) {
			continue
		}
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, id+".jsonl")
		f, err := privatefile.Open(path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_RDWR)
		if err != nil {
			writer.Close()
			if os.IsExist(err) {
				continue
			}
			return nil, err
		}
		if preflight != nil {
			if err := preflight(f, true); err != nil {
				f.Close()
				writer.Close()
				return nil, err
			}
		}
		return &Store{ID: id, Path: path, file: f, writer: writer, stopHeartbeat: startWriterHeartbeat(writer)}, nil
	}
}

func newStore(id string, existing bool, preflight func(*os.File, bool) error) (*Store, error) {
	if id != "" {
		if err := ValidateSessionID(id); err != nil {
			return nil, err
		}
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if id == "" {
		return createAutoStore(dir, time.Now().Format("20060102-150405"), preflight)
	}
	writer, err := acquireWriter(id)
	if err != nil {
		return nil, err
	}
	store, err := finishStore(id, existing, writer, preflight)
	if err != nil {
		writer.Close()
		return nil, err
	}
	return store, nil
}

// finishStore does not close writer. The caller closes it on failure; on
// success the returned store owns it.
func finishStore(id string, existing bool, writer *os.File, preflight func(*os.File, bool) error) (*Store, error) {
	path := filepath.Join(Dir(), id+".jsonl")
	// Windows append handles cannot truncate, even with O_RDWR. Repair
	// through a separate read/write handle, then reopen for append-only use.
	if err := checkTranscriptPath(path); err != nil {
		return nil, err
	}
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)
	flags := os.O_RDWR
	if !existing {
		flags |= os.O_CREATE
	}
	f, err := privatefile.Open(path, flags)
	if err != nil {
		return nil, err
	}
	if err := checkTranscriptLinks(f); err != nil {
		f.Close()
		return nil, err
	}
	if preflight != nil {
		if err := preflight(f, created); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.Seek(0, 0); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := recoverTail(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	f, err = privatefile.Open(path, os.O_APPEND|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	return &Store{ID: id, Path: path, file: f, writer: writer, stopHeartbeat: startWriterHeartbeat(writer)}, nil
}

func (s *Store) Append(m llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockManagement()
	if err != nil {
		return err
	}
	defer unlock()

	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	m, err = persistImages(m)
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b)+1 >= maxRecordBytes {
		return fmt.Errorf("session message exceeds the %d-byte record limit", maxRecordBytes)
	}
	if err := s.writeRecord(b); err != nil {
		return err
	}
	s.acknowledgeOutputs(m)
	return nil
}

// writeRecord runs under mu. A successful append is synced before the caller
// may dispatch a side effect depending on that record.
func (s *Store) writeRecord(b []byte) error {
	unlock, err := lockTranscript(s.file)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.file.Write(append(b, '\n')); err != nil {
		return err
	}
	return s.file.Sync()
}

// clearMarkerRole marks a /clear point in the JSONL. Everything before
// the last marker is kept in the file but never reloaded by Load.
const clearMarkerRole = "__clear__"

// compactMarkerRole marks a /compact point. Load returns only the summary
// message written after it (plus later messages).
const compactMarkerRole = "__compact__"

const maxRecordBytes = 4 * 1024 * 1024

// Clear records a clear point in the session file. The caller also drops
// its in-memory history; Load after this returns only later messages.
func (s *Store) Clear() error {
	return s.Append(llm.Message{Role: clearMarkerRole})
}

// MarkCompact commits a summary checkpoint, which becomes the context on Load.
func (s *Store) MarkCompact(summary string) error {
	return s.MarkCompactMessages([]llm.Message{{Role: "user", Kind: "summary", Content: "Previous conversation summary:\n" + summary}})
}

// MarkCompactMessages commits replacement context (a summary or pruned history)
// in a single record.
func (s *Store) MarkCompactMessages(msgs []llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockManagement()
	if err != nil {
		return err
	}
	defer unlock()

	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	msgs, err = persistMessageImages(msgs)
	if err != nil {
		return err
	}
	b, err := marshalCheckpoint(msgs)
	if err != nil {
		return err
	}
	return s.writeRecord(b)
}

// CheckpointFits reports whether a replacement can be loaded as one session
// record. Large raw histories should be summarized instead of pruned.
func CheckpointFits(msgs []llm.Message) bool {
	msgs, err := ImageReferences(msgs)
	if err != nil {
		return false
	}
	_, err = marshalCheckpoint(msgs)
	return err == nil
}

func marshalCheckpoint(msgs []llm.Message) ([]byte, error) {
	b, err := json.Marshal(struct {
		Role     string        `json:"role"`
		Messages []llm.Message `json:"messages"`
	}{compactMarkerRole, msgs})
	if err != nil {
		return nil, err
	}
	if len(b)+1 >= maxRecordBytes {
		return nil, fmt.Errorf("session checkpoint exceeds the %d-byte record limit", maxRecordBytes)
	}
	return b, nil
}

// ArchiveCopy copies the current session file to a new file in the same
// directory and returns its path. Used before compaction so the original
// transcript is kept separately.
func (s *Store) ArchiveCopy(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && s.file == nil {
		return "", ErrOperationPending
	}
	unlock, err := lockManagement()
	if err != nil {
		return "", err
	}
	defer unlock()
	if s.closed {
		release, err := s.lockClosedStore()
		if err != nil {
			return "", err
		}
		defer release()
	}
	// Archives add a suffix to a valid (up to 128-byte) session ID.
	if err := validateSessionName(name, 240); err != nil {
		return "", err
	}
	if _, err := recoverOperations(s.ID, name); err != nil {
		return "", err
	}
	writer, err := acquireWriterFile(name)
	if err != nil {
		return "", err
	}
	defer writer.Close()
	source, err := os.OpenFile(s.Path, os.O_RDONLY|metadataReadFlags, 0)
	if err != nil {
		return "", err
	}
	defer source.Close()
	if st, err := source.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("archive source must be regular: %v", err)
	}
	dst := filepath.Join(Dir(), name+".jsonl")
	f, err := privatefile.Open(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return "", err
	}
	if err := s.registerArchiveLocked(name + ".jsonl"); err != nil {
		f.Close()
		return "", errors.Join(err, os.Remove(dst))
	}
	_, writeErr := io.Copy(f, source)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", err
	}
	if err := syncRecoveryDirectory(Dir()); err != nil {
		return "", err
	}
	return dst, nil
}

// IsTurnStart reports whether m starts a user turn: a user message
// with no images that is not a turn-internal message the loop
// itself appended. Turn-internal user messages include loaded skill
// bodies ("Skill <name>:\n..."), background-job deliveries
// ("Background job ...") and the compact summary ("Previous
// conversation summary:\n..."). Kind distinguishes new task messages
// explicitly; prefix checks exist only for older transcripts.
func IsTurnStart(m llm.Message) bool {
	if m.Kind != "" {
		return m.Kind == "task"
	}
	if m.Role != "user" || len(m.Images) > 0 || strings.TrimSpace(m.Content) == "" {
		return false
	}
	for _, p := range []string{"Skill ", "Background job ", "Previous conversation summary:"} {
		if strings.HasPrefix(m.Content, p) {
			return false
		}
	}
	return true
}

// Rollback drops the last n turns from msgs and returns the kept
// prefix and how many messages were dropped. A turn is the messages
// from one turn-start user message up to (not including) the next.
// n <= 0 drops nothing; n >= the number of turns drops every turn,
// keeping only the pre-turn prefix (usually a compact summary, or
// nothing). It never mutates msgs.
func Rollback(msgs []llm.Message, n int) ([]llm.Message, int) {
	if n <= 0 {
		return msgs, 0
	}
	var starts []int
	for i, m := range msgs {
		if IsTurnStart(m) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return msgs, 0
	}
	cut := 0
	if n < len(starts) {
		cut = starts[len(starts)-n]
	} else {
		cut = starts[0]
	}
	kept := make([]llm.Message, cut)
	copy(kept, msgs[:cut])
	return kept, len(msgs) - cut
}

// Rewrite replaces the session file's contents with msgs, keeping
// the store usable for further appends. The caller archives first
// (ArchiveCopy): a rewrite is a real deletion, and the archive is
// the recovery path — the same shape as compaction, which archives
// before replacing history with a summary.
func (s *Store) Rewrite(msgs []llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockManagement()
	if err != nil {
		return err
	}
	defer unlock()

	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	msgs, err = persistMessageImages(msgs)
	if err != nil {
		return err
	}
	var b strings.Builder
	if s.header != nil {
		line, err := json.Marshal(s.header)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	for _, m := range msgs {
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".session-rewrite-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		s.file, _ = privatefile.Open(s.Path, os.O_APPEND|os.O_RDWR)
		return err
	}
	f, err := privatefile.Open(s.Path, os.O_APPEND|os.O_RDWR)
	if err != nil {
		return err
	}
	s.file = f
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.auditFile != nil {
		s.auditFile.Close()
		s.auditFile = nil
	}
	var err error
	if s.file != nil {
		err = s.file.Close()
	}
	if s.stopHeartbeat != nil {
		s.stopHeartbeat()
		s.stopHeartbeat = nil
	}
	if s.writer != nil {
		err = errors.Join(err, s.writer.Close())
		s.writer = nil
	}
	if s.outputWriter != nil {
		err = errors.Join(err, s.outputWriter.Close())
		s.outputWriter = nil
	}
	return err
}

// OutputID returns the stable output-family identity for this store.
func (s *Store) OutputID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.header != nil && s.header.OutputID != "" {
		return s.header.OutputID
	}
	return s.ID
}

// OutputDir follows the persistent output identity, including for old sessions
// whose original directory is adopted on first open with this version.
func (s *Store) OutputDir() string {
	return filepath.Join(Dir(), s.OutputID(), "bash-output")
}

// JobsDir returns the durable background-job metadata directory owned by this
// session's persistent output identity.
func (s *Store) JobsDir() string {
	return filepath.Join(Dir(), s.OutputID(), "bash-output-jobs")
}

// Load replays the active checkpoint and repairs incomplete tool batches
// without replaying actions whose outcome was not recorded.
func Load(id string) ([]llm.Message, error) {
	if err := ValidateSessionID(id); err != nil {
		return nil, err
	}
	path := filepath.Join(Dir(), id+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open session %s: %w", id, err)
	}
	defer f.Close()
	var msgs []llm.Message
	var replayErr error
	seenRecord := false
	_, _, _, err = scanRecords(f, func(line []byte, m llm.Message) {
		if replayErr != nil {
			return
		}
		first := !seenRecord
		seenRecord = true
		if m.Role == headerRole {
			if !first {
				replayErr = fmt.Errorf("session header must be the first record")
				return
			}
			_, replayErr = decodeHeader(line)
			return
		}
		if m.Role == invocationMarkerRole {
			replayErr = applyInvocation(msgs, m.ToolInvocations)
			return
		}
		if m.Role == clearMarkerRole || m.Role == compactMarkerRole {
			// /clear or /compact point: forget everything before it
			// (file keeps it).
			msgs = nil
			if m.Role == compactMarkerRole {
				var checkpoint struct {
					Messages []llm.Message `json:"messages"`
				}
				if err := json.Unmarshal(line, &checkpoint); err == nil && checkpoint.Messages != nil {
					msgs = checkpoint.Messages
					return
				}
			}
			if m.Role == compactMarkerRole && m.Content != "" {
				m.Role = "user"
				m.Kind = "summary"
				msgs = append(msgs, m)
			}
			return
		}
		msgs = append(msgs, m)
	})
	if err != nil {
		return nil, err
	}
	if replayErr != nil {
		return nil, fmt.Errorf("load session invocation: %w", replayErr)
	}
	return RepairToolBatches(msgs), nil
}

// RepairToolBatches preserves completed work and supplies explicit unknown-
// outcome observations for calls interrupted before their results were saved.
// A recovered call is never replayed: it might already have caused a side effect.
func RepairToolBatches(msgs []llm.Message) []llm.Message {
	var out []llm.Message
	for i := 0; i < len(msgs); {
		m := msgs[i]
		i++
		if m.Role == "tool" {
			continue
		} // orphaned results cannot be sent to a provider
		if m.Role == "assistant" && (m.FinishReason == "length" || m.FinishReason == "max_tokens" || m.FinishReason == "incomplete") {
			// These arguments were never dispatched. A partially generated
			// name or JSON object must not poison the next provider request.
			m.ToolCalls = nil
			m.AnthropicContent = nil
			if m.Content == "" {
				m.Content = "[The model output was truncated before its tool calls were complete. No tools from this response were executed.]"
			}
		}
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		results := map[string]llm.Message{}
		var deferred []llm.Message
		for i < len(msgs) {
			next := msgs[i]
			if next.Role == "assistant" || (next.Role == "user" && IsTurnStart(next)) {
				break
			}
			i++
			if next.Role == "tool" {
				results[next.ToolCallID] = next
			} else {
				deferred = append(deferred, next)
			}
		}
		for _, call := range m.ToolCalls {
			result, ok := results[call.ID]
			if !ok {
				result = llm.Message{Role: "tool", ToolCallID: call.ID, ToolError: true, Kind: "interrupted", Content: "Error: execution was interrupted before this result was recorded. The outcome is unknown; inspect the current state before retrying any action."}
			}
			if result.InvocationID == "" {
				for _, inv := range m.ToolInvocations {
					if inv.ToolCallID == call.ID {
						result.InvocationID = inv.ID
						break
					}
				}
			}
			out = append(out, result)
		}
		out = append(out, deferred...)
	}
	return out
}
