// Package workspace identifies workspace directories and stores explicit trust
// decisions outside the project. Project files cannot grant their own trust.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Identity struct {
	Path   string `json:"path"`
	FileID string `json:"file_id"`
}

func Identify(path string) (Identity, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Identity{}, err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Identity{}, err
	}
	id, err := directoryID(canonical)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Path: canonical, FileID: id}, nil
}

func trustDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".minimal-harness", "trust"), nil
}

func trustName(id Identity) string {
	sum := sha256.Sum256([]byte(id.Path))
	return hex.EncodeToString(sum[:]) + ".json"
}

type trustRecord struct {
	Version   int      `json:"version"`
	Workspace Identity `json:"workspace"`
}

func Trusted(id Identity) (bool, error) {
	dir, err := trustDir()
	if err != nil {
		return false, err
	}
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	name := trustName(id)
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("workspace trust record is not a regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|trustReadFlags, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("workspace trust record is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return false, err
	}
	var record trustRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("invalid workspace trust record: %w", err)
	}
	return record.Version == 1 && record.Workspace == id, nil
}

// Decide accepts only a direct user action, never a value from project config.
// Each directory has an independent atomic record, so concurrent decisions for
// different workspaces cannot lose each other's updates.
func Decide(id Identity, action string) error {
	if action != "trust" && action != "untrust" {
		return fmt.Errorf("workspace-trust must be trust or untrust")
	}
	dir, err := trustDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	name := filepath.Join(dir, trustName(id))
	if action == "untrust" {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncDirectory(dir)
	}
	data, err := json.Marshal(trustRecord{Version: 1, Workspace: id})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".trust-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), name); err != nil {
		return err
	}
	return syncDirectory(dir)
}
