package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"minimalharness/internal/privatefile"
	"minimalharness/internal/session"
)

// replayLog appends encoded events for one RPC connection so a later process
// can resume from after_sequence. The file lives next to the session transcript.
type replayLog struct {
	path string
	file *os.File
}

func replayLogPath(sessionID string) (string, error) {
	if err := session.ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(session.Dir(), "rpc-replay", sessionID+".jsonl"), nil
}

func openReplayLog(sessionID string) (*replayLog, error) {
	path, err := replayLogPath(sessionID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := privatefile.Open(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	return &replayLog{path: path, file: f}, nil
}

func (l *replayLog) append(data []byte, sync bool) error {
	if l == nil || l.file == nil {
		return errors.New("replay log is closed")
	}
	line := append(append([]byte(nil), data...), '\n')
	if _, err := l.file.Write(line); err != nil {
		return err
	}
	if !sync {
		return nil
	}
	return l.file.Sync()
}

func (l *replayLog) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// loadReplayLog reads a connection's durable events. A truncated final line is
// ignored so a crash mid-write does not drop the prefix. Events larger than
// maxReplayEventSize are reported as gaps and do not erase earlier records.
func loadReplayLog(sessionID, connectionID string) (events []replayEvent, evicted uint64, oversized []uint64, err error) {
	path, err := replayLogPath(sessionID)
	if err != nil {
		return nil, 0, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil, nil
		}
		return nil, 0, nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxOutputFrameBytes+1)
	var bytes int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var head struct {
			Sequence     uint64 `json:"sequence"`
			ConnectionID string `json:"connection_id"`
			Type         string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			continue
		}
		if head.ConnectionID != connectionID || head.Sequence == 0 {
			continue
		}
		if head.Type == "replay_omitted" || len(line) > maxReplayEventSize {
			oversized = append(oversized, head.Sequence)
			if head.Sequence > evicted {
				evicted = head.Sequence
			}
			continue
		}
		copied := append([]byte(nil), line...)
		var dropped uint64
		events, bytes, dropped = admitReplayEvent(events, bytes, replayEvent{sequence: head.Sequence, data: copied}, criticalReplayType(copied))
		if dropped > evicted {
			evicted = dropped
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return events, evicted, oversized, err
	}
	return events, evicted, oversized, nil
}

func moveReplayLog(oldID, newID string) error {
	if err := session.ValidateSessionID(oldID); err != nil {
		return err
	}
	if err := session.ValidateSessionID(newID); err != nil {
		return err
	}
	oldPath, err := replayLogPath(oldID)
	if err != nil {
		return err
	}
	newPath, err := replayLogPath(newID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("replay log %s already exists", newID)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(oldPath, newPath)
}

func removeReplayLog(sessionID string) error {
	path, err := replayLogPath(sessionID)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
