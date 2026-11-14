// Package session appends one JSON line per message to a session file
// and reloads it for --resume, repairing interrupted tool batches.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/llm"
)

type Store struct {
	ID        string
	Path      string
	file      *os.File
	auditFile *os.File // lazily opened sidecar for audit records
	// mu guards Append and Audit: a background job's completion audit
	// can land on its own goroutine while the loop appends or audits
	// on the main one.
	mu     sync.Mutex
	closed bool
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
	if s.closed {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(s.AuditPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(b, '\n'))
		return err
	}
	if s.auditFile == nil {
		f, err := os.OpenFile(s.AuditPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
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

func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".minimal-harness", "sessions")
}

func New(id string) (*Store, error) {
	auto := id == ""
	if !auto {
		if err := ValidateSessionID(id); err != nil {
			return nil, err
		}
	}
	if auto {
		id = time.Now().Format("20060102-150405")
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".jsonl")
	if auto {
		// Two sessions started in the same second (or a new_session
		// right after startup) must not share one file: suffix
		// until the path is free. An explicit id is never suffixed:
		// --resume needs the exact file.
		for i := 1; ; i++ {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0600)
			if err == nil {
				return &Store{ID: id, Path: path, file: f}, nil
			}
			if !os.IsExist(err) {
				return nil, err
			}
			id = fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), i)
			path = filepath.Join(dir, id+".jsonl")
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Store{ID: id, Path: path, file: f}, nil
}

func (s *Store) Append(m llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.file.Write(append(b, '\n'))
	return err
}

// clearMarkerRole marks a /clear point in the JSONL. Everything before
// the last marker is kept in the file but never reloaded by Load.
const clearMarkerRole = "__clear__"

// compactMarkerRole marks a /compact point. Load returns only the summary
// message written after it (plus later messages).
const compactMarkerRole = "__compact__"

// Clear records a clear point in the session file. The caller also drops
// its in-memory history; Load after this returns only later messages.
func (s *Store) Clear() error {
	return s.Append(llm.Message{Role: clearMarkerRole})
}

// MarkCompact commits a summary checkpoint, which becomes the context on Load.
func (s *Store) MarkCompact(summary string) error {
	return s.MarkCompactMessages([]llm.Message{{Role: "user", Kind: "summary", Content: "Previous conversation summary:\n" + summary}})
}

// MarkCompactMessages commits the summary and active task in a single record.
func (s *Store) MarkCompactMessages(msgs []llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	b, err := json.Marshal(struct {
		Role     string        `json:"role"`
		Messages []llm.Message `json:"messages"`
	}{compactMarkerRole, msgs})
	if err != nil {
		return err
	}
	_, err = s.file.Write(append(b, '\n'))
	return err
}

// ArchiveCopy copies the current session file to a new file in the same
// directory and returns its path. Used before compaction so the original
// transcript is kept separately.
func (s *Store) ArchiveCopy(name string) (string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(Dir(), name+".jsonl")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// IsTurnStart reports whether m starts a user turn: a user message
// with no images that is not a turn-internal message the loop
// itself appended. Turn-internal user messages are the auto-loaded
// skill bodies ("Skill <name>:\n..."), background-job deliveries
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
	if s.closed || s.file == nil {
		return os.ErrClosed
	}
	var b strings.Builder
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
		s.file, _ = os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0600)
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0600)
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
	return s.file.Close()
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
			// /clear or /compact point: forget everything before it
			// (file keeps it).
			msgs = nil
			if m.Role == compactMarkerRole {
				var checkpoint struct {
					Messages []llm.Message `json:"messages"`
				}
				if err := json.Unmarshal(line, &checkpoint); err == nil && checkpoint.Messages != nil {
					msgs = checkpoint.Messages
					continue
				}
			}
			if m.Role == compactMarkerRole && m.Content != "" {
				m.Role = "user"
				m.Kind = "summary"
				msgs = append(msgs, m)
			}
			continue
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
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
			out = append(out, result)
		}
		out = append(out, deferred...)
	}
	return out
}
