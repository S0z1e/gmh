package rpc

import (
	"bufio"
	"bytes"
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

func replayDirectory() (string, error) {
	if err := os.MkdirAll(session.Dir(), 0700); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(session.Dir())
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := root.Mkdir("rpc-replay", replayDirMode()); err != nil && !os.IsExist(err) {
		return "", err
	}
	st, err := root.Lstat("rpc-replay")
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return "", fmt.Errorf("rpc replay directory must not be an alias")
	}
	return filepath.Join(session.Dir(), "rpc-replay"), nil
}

func openReplayLog(sessionID string) (*replayLog, error) {
	path, err := replayLogPath(sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := replayDirectory(); err != nil {
		return nil, err
	}
	f, unlock, err := openReplayFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND)
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
	return l.appendLimited(data, syncWrite, false)
}

func (l *replayLog) appendCap(data []byte) error {
	err := l.appendLimited(data, true, true)
	l.full = true
	return err
}

func (l *replayLog) appendLimited(data []byte, syncWrite, capMarker bool) error {
	if l == nil || l.file == nil {
		return errors.New("replay log is closed")
	}
	if l.full {
		return errReplayLogFull
	}
	line := append(append([]byte(nil), data...), '\n')
	limit := int64(maxReplayLogBytes - 256)
	if capMarker {
		limit = maxReplayLogBytes
	}
	unlock, err := lockReplay(l.file)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := l.file.Stat()
	if err != nil {
		return err
	}
	l.bytes = info.Size()
	if l.bytes+int64(len(line)) > limit {
		return errReplayLogFull
	}
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
	// O_RDWR: Windows LockFileEx rejects a read-only handle.
	f, unlock, err := openReplayFile(path, os.O_RDWR)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil, nil
		}
		return nil, 0, nil, err
	}
	defer unlock()
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64*1024)
	var stored int
	var last uint64
	for {
		line, torn, tooLong, err := readReplayLine(reader)
		if torn {
			break
		}
		if tooLong {
			gapAfter(&evicted, last)
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return events, evicted, oversized, err
		}
		if len(line) == 0 {
			continue
		}
		var head struct {
			Sequence     uint64 `json:"sequence"`
			ConnectionID string `json:"connection_id"`
			Type         string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			gapAfter(&evicted, last)
			continue
		}
		if head.Type == "replay_capped" {
			raiseEvicted(&evicted, head.Sequence)
			continue
		}
		if head.ConnectionID != connectionID || head.Sequence == 0 {
			continue
		}
		if head.Sequence > last {
			last = head.Sequence
		}
		if head.Type == "replay_omitted" || len(line) > maxReplayEventSize {
			oversized = append(oversized, head.Sequence)
			raiseEvicted(&evicted, head.Sequence)
			continue
		}
		copied := append([]byte(nil), line...)
		var dropped uint64
		events, stored, dropped = admitReplayEvent(events, stored, replayEvent{sequence: head.Sequence, data: copied, critical: criticalReplayType(copied)})
		raiseEvicted(&evicted, dropped)
	}
	return events, evicted, oversized, nil
}

func raiseEvicted(evicted *uint64, seq uint64) {
	if seq > *evicted {
		*evicted = seq
	}
}

func gapAfter(evicted *uint64, last uint64) {
	if last == 0 {
		raiseEvicted(evicted, 1)
		return
	}
	if last == ^uint64(0) {
		raiseEvicted(evicted, last)
		return
	}
	raiseEvicted(evicted, last+1)
}

// readReplayLine returns one LF-terminated record. A final line without LF is
// torn and ignored. A line longer than the frame limit is discarded.
func readReplayLine(r *bufio.Reader) (line []byte, torn, tooLong bool, err error) {
	var buf []byte
	for {
		part, readErr := r.ReadSlice('\n')
		if len(buf)+len(part) > maxOutputFrameBytes {
			tooLong = true
		}
		if !tooLong {
			buf = append(buf, part...)
		}
		if readErr == nil {
			if tooLong {
				return nil, false, true, nil
			}
			return bytes.TrimRight(buf, "\r\n"), false, false, nil
		}
		if errors.Is(readErr, bufio.ErrTooLong) {
			tooLong = true
			continue
		}
		if errors.Is(readErr, io.EOF) {
			if tooLong {
				return nil, false, true, nil
			}
			if len(part) == 0 && len(buf) == 0 {
				return nil, false, false, io.EOF
			}
			return nil, true, false, nil
		}
		return nil, false, false, readErr
	}
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
