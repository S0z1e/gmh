// Package session appends one JSON line per message to a session file
// and reloads it for --resume. No compaction in v1; the loop caps steps.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"minimalharness/internal/llm"
)

type Store struct {
	ID   string
	Path string
	file *os.File
}

func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".minimal-harness", "sessions")
}

func New(id string) (*Store, error) {
	if id == "" {
		id = time.Now().Format("20060102-150405")
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Store{ID: id, Path: path, file: f}, nil
}

func (s *Store) Append(m llm.Message) error {
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

// Clear records a clear point in the session file. The caller also drops
// its in-memory history; Load after this returns only later messages.
func (s *Store) Clear() error {
	return s.Append(llm.Message{Role: clearMarkerRole})
}

func (s *Store) Close() error { return s.file.Close() }

// Load replays a JSONL session. A trailing dangling assistant tool call
// (tool call with no matching tool result) is dropped so resume does not
// break on an incomplete turn.
func Load(id string) ([]llm.Message, error) {
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
		if m.Role == clearMarkerRole {
			// /clear point: forget everything before it (file keeps it).
			msgs = nil
			continue
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Drop a dangling final assistant message that has tool calls but no
	// following tool result.
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last.Role == "assistant" && len(last.ToolCalls) > 0 {
			msgs = msgs[:len(msgs)-1]
		}
	}
	return msgs, nil
}
