package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"minimalharness/internal/llm"
	"minimalharness/internal/privatefile"
)

// scanRecords ignores only a damaged, unterminated final record. Complete
// corrupt records are errors, including records before a clear/checkpoint.
// end is the end of the valid prefix; newline reports a valid final record
// that needs its delimiter restored before appending.
func scanRecords(f io.Reader, visit func([]byte, llm.Message)) (end int64, newline, torn bool, err error) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxRecordBytes)
	var advance int
	var terminated bool
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		n, token, splitErr := bufio.ScanLines(data, atEOF)
		advance = n
		terminated = n > 0 && data[n-1] == '\n'
		return n, token, splitErr
	})
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) != 0 {
			var m llm.Message
			if decodeErr := json.Unmarshal(line, &m); decodeErr != nil {
				if !terminated {
					if readErr := sc.Err(); readErr != nil {
						return end, false, false, readErr
					}
					return end, false, true, nil
				}
				return end, false, false, fmt.Errorf("corrupt session record at byte %d: %w", end, decodeErr)
			}
			if visit != nil {
				visit(line, m)
			}
		}
		end += int64(advance)
		newline = !terminated
	}
	return end, newline, false, sc.Err()
}

func recoverTail(f *os.File) error {
	unlock, err := lockTranscript(f)
	if err != nil {
		return err
	}
	defer unlock()
	end, newline, torn, err := scanRecords(f, nil)
	if err != nil {
		return err
	}
	if torn {
		backup, err := privatefile.Open(f.Name()+"-torn-"+rand.Text()+".bak", os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return err
		}
		defer backup.Close()
		if _, err := f.Seek(end, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(backup, f); err != nil {
			return err
		}
		if err := backup.Sync(); err != nil {
			return err
		}
		if err := syncRecoveryDirectory(filepath.Dir(f.Name())); err != nil {
			return err
		}
		if err := f.Truncate(end); err != nil {
			return err
		}
	}
	if newline {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	if torn || newline {
		return f.Sync()
	}
	return nil
}

// Match basenames literally: HOME may contain glob metacharacters. The .bak
// suffix distinguishes recovery files from independently named .jsonl sessions.
func recoveryBackups(path string) ([]string, error) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(path) + "-torn-"
	var backups []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".bak") && validBackupID(strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), ".bak")) {
			backups = append(backups, filepath.Join(dir, entry.Name()))
		}
	}
	return backups, nil
}

// rand.Text uses the RFC 4648 base32 alphabet. Restricting the nonce prevents
// another valid session ID (and its backups) from matching this session family.
func validBackupID(id string) bool {
	if id == "" {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'A' && ch <= 'Z') && !(ch >= '2' && ch <= '7') {
			return false
		}
	}
	return true
}
