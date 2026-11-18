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

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type Identity struct {
	Path   string `json:"path"`
	FileID string `json:"file_id"`
}

func Identify(path string) (Identity, error) { return identify(path) }

// CheckRootFileID verifies the incarnation of a directory already opened by a caller.
func CheckRootFileID(root *os.Root, expected string) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	id, err := DirectoryFileID(f)
	if err != nil {
		return err
	}
	if id == "" || id != expected {
		return fmt.Errorf("workspace directory identity changed")
	}
	return nil
}

// Workspace binding paths can preserve some case/Unicode aliases, including UNC volumes.
// Fold only the lookup key, and always pair it with the directory incarnation:
// distinct directories on case-sensitive filesystems retain distinct records.
// This does not require read permission on otherwise searchable ancestors.
func pathKey(path string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(filepath.ToSlash(path))))
}

// Matches requires the same canonical location and directory incarnation.
// Without a creation identity, callers must ask for an explicit new binding.
func (a Identity) Matches(b Identity) bool { return sameIdentity(a, b) }

func sameIdentity(a, b Identity) bool {
	return a.FileID != "" && a.FileID == b.FileID && pathKey(a.Path) == pathKey(b.Path)
}

func trustDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".minimal-harness", "trust"), nil
}

func trustName(id Identity) string {
	sum := sha256.Sum256([]byte(pathKey(id.Path) + "\x00" + id.FileID))
	return hex.EncodeToString(sum[:]) + ".json"
}

type trustRecord struct {
	Version   int      `json:"version"`
	Workspace Identity `json:"workspace"`
}

func Trusted(id Identity) (bool, error) {
	if id.FileID == "" {
		return false, nil // No stable incarnation available: never reuse trust.
	}
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
	const maxTrustBytes = 64 << 10
	data, err := io.ReadAll(io.LimitReader(f, maxTrustBytes+1))
	if err != nil {
		return false, err
	}
	if len(data) > maxTrustBytes {
		return false, fmt.Errorf("workspace trust record exceeds %d bytes", maxTrustBytes)
	}
	var record trustRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("invalid workspace trust record: %w", err)
	}
	return record.Version == 2 && sameIdentity(record.Workspace, id), nil
}

// Decide accepts only a direct user action, never a value from project config.
// Each directory has an independent atomic record, so concurrent decisions for
// different workspaces cannot lose each other's updates.
func Decide(id Identity, action string) error {
	if action != "trust" && action != "untrust" {
		return fmt.Errorf("workspace-trust must be trust or untrust")
	}
	if action == "trust" && id.FileID == "" {
		return fmt.Errorf("cannot persist workspace trust: this filesystem does not expose a directory creation identity")
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
	data, err := json.Marshal(trustRecord{Version: 2, Workspace: id})
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
