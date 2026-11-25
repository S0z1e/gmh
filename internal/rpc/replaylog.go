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
	path  string
	file  *os.File
	bytes int64
	full  bool
}

func replayLogPath(sessionID string) (string, error) {
	if err := session.ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(session.Dir(), "rpc-replay", sessionID+".jsonl"), nil
}

const maxReplayLogBytes = 64 << 20

func openReplayFile(path string, flags int) (*os.File, func(), error) {
	if err := checkReplayPath(path); err != nil {
		return nil, nil, err
	}
	f, err := privatefile.Open(path, flags)
	if err != nil {
		return nil, nil, err
	}
	actual, statErr := f.Stat()
	named, nameErr := os.Lstat(path)
	if statErr != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(actual, named) {
		f.Close()
		return nil, nil, fmt.Errorf("rpc replay log must be a regular, unaliased file")
	}
	if err := checkReplayLinks(f); err != nil {
		f.Close()
		return nil, nil, err
	}
	unlock, err := lockReplay(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, unlock, nil
}

func openReplayLog(sessionID string) (*replayLog, error) {
	path, err := replayLogPath(sessionID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), replayDirMode()); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), replayDirMode()); err != nil {
		return nil, err
	}
	f, unlock, err := openReplayFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		unlock()
		f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		unlock()
		f.Close()
		return nil, err
	}
	unlock()
	return &replayLog{path: path, file: f, bytes: info.Size(), full: info.Size() >= maxReplayLogBytes}, nil
}

func (l *replayLog) append(data []byte, syncWrite bool) error {
	if l == nil || l.file == nil {
		return errors.New("replay log is closed")
	}
	if l.full {
		return errReplayLogFull
	}
	line := append(append([]byte(nil), data...), '\n')
	if l.bytes+int64(len(line)) > maxReplayLogBytes {
		l.full = true
		return errReplayLogFull
	}
	unlock, err := lockReplay(l.file)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := l.file.Write(line); err != nil {
		return err
	}
	l.bytes += int64(len(line))
	if !syncWrite {
		return nil
	}
	return l.file.Sync()
}

var errReplayLogFull = errors.New("rpc replay log is full")

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
	f, unlock, err := openReplayFile(path, os.O_RDONLY)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil, nil
		}
		return nil, 0, nil, err
	}
	defer unlock()
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
			// A corrupt record is a gap at this file position. The final torn
			// line is handled after the scan, so a prefix stays contiguous.
			return events, headGap(events), oversized, nil
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
		events, bytes, dropped = admitReplayEvent(events, bytes, replayEvent{sequence: head.Sequence, data: copied, critical: criticalReplayType(copied)})
		if dropped > evicted {
			evicted = dropped
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return events, headGap(events), oversized, nil
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			return events, evicted, oversized, err
		}
	}
	return events, evicted, oversized, nil
}

func headGap(events []replayEvent) uint64 {
	if len(events) == 0 {
		return 1
	}
	return events[len(events)-1].sequence
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
	oldInfo, err := os.Lstat(oldPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := checkReplayPath(oldPath); err != nil {
		return err
	}
	if newInfo, err := os.Lstat(newPath); err == nil {
		if os.SameFile(oldInfo, newInfo) {
			return os.Rename(oldPath, newPath)
		}
		if newInfo.Mode()&os.ModeSymlink != 0 || !newInfo.Mode().IsRegular() {
			return fmt.Errorf("replay log %s is not a regular file", newID)
		}
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
