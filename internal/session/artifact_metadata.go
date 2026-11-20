package session

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// Metadata directories must never alias another artifact family. Mutations
// remain rooted even if an external process replaces a path while we operate.
func metadataDirectory(name string, create bool) (string, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return "", fmt.Errorf("invalid metadata directory")
	}
	root, err := os.OpenRoot(Dir())
	if err != nil {
		return "", err
	}
	defer root.Close()
	if create {
		if err := root.Mkdir(name, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	st, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("metadata directory must not be an alias")
	}
	if create {
		if err := syncRecoveryDirectory(Dir()); err != nil {
			return "", err
		}
	}
	return filepath.Join(Dir(), name), nil
}
func writeArtifactMetadata(dir, name string, data []byte) error {
	path, err := metadataDirectory(dir, true)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	temp := ".metadata-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		return err
	}
	return syncRecoveryDirectory(path)
}
