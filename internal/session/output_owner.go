package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func randomOutputID() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("output-%x", token), nil
}

// The output lease is also a durable ownership claim. Transcript names can be
// reused after rename, but a different legacy transcript cannot adopt that old
// directory merely because its live writer has closed. Creation time survives
// rename/rebind and identifies retries of an interrupted first adoption.
func acquireOutputWriter(h *Header, legacyID string) (*os.File, error) {
	unassigned := h.OutputID == ""
	id := h.OutputID
	if unassigned {
		id = legacyID
	}
	for {
		f, err := acquireWriterIn(filepath.Join(Dir(), ".output-writers"), id)
		if err == nil {
			err = claimOutputWriter(f, h.Created)
			if err == nil {
				h.OutputID = id
				return f, nil
			}
			_ = f.Close()
		}
		if !unassigned || !errors.Is(err, ErrSessionBusy) {
			return nil, err
		}
		// A legacy name is now owned by another session. Keep adopting the
		// transcript, but allocate its own output identity instead.
		id, err = randomOutputID()
		if err != nil {
			return nil, err
		}
	}
}

func claimOutputWriter(f *os.File, created time.Time) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("output ownership lease must be a regular file")
	}
	if err := checkTranscriptLinks(f); err != nil {
		return err
	}
	claim := created.UTC().Format(time.RFC3339Nano) + "\n"
	if info.Size() == 0 {
		if _, err := f.WriteAt([]byte(claim), 0); err != nil {
			return err
		}
		return f.Sync()
	}
	if info.Size() > 64 {
		return fmt.Errorf("invalid output ownership claim")
	}
	data := make([]byte, int(info.Size()))
	if _, err := f.ReadAt(data, 0); err != nil {
		return err
	}
	if len(data) < 2 || data[len(data)-1] != '\n' {
		return fmt.Errorf("invalid output ownership claim")
	}
	if _, err := time.Parse(time.RFC3339Nano, string(data[:len(data)-1])); err != nil {
		return fmt.Errorf("invalid output ownership claim: %w", err)
	}
	if string(data) != claim {
		return fmt.Errorf("output directory belongs to another session: %w", ErrSessionBusy)
	}
	return nil
}
