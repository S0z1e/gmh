// Package privatefile opens harness-owned transcripts and logs for owner-only use.
package privatefile

import "os"

func Open(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func Write(path string, data []byte) error {
	f, err := Open(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
