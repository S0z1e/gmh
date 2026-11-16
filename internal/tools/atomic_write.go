package tools

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// atomicWriteFile follows file symlinks within r, then stages and renames
// through one pinned parent directory. No operation reopens an ordinary path.
func atomicWriteFile(r *os.Root, name string, data []byte) error {
	var mode os.FileMode
	for links := 0; ; links++ {
		st, err := r.Lstat(name)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink == 0 {
			if !st.Mode().IsRegular() {
				return fmt.Errorf("cannot replace non-regular file: %s", name)
			}
			mode = st.Mode().Perm()
			break
		}
		if links >= 40 {
			return fmt.Errorf("too many symlinks: %s", name)
		}
		target, err := r.Readlink(name)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			return fmt.Errorf("absolute symlink target is outside workspace root: %s", target)
		}
		// Keep .. components intact: a preceding directory may itself be
		// a symlink, so lexical cleaning would change the target.
		dir, _ := filepath.Split(name)
		name = dir + target
	}
	dir, base := filepath.Split(name)
	if dir == "" {
		dir = "."
	}
	parent, err := r.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	tmpName := ".gmh-write-" + rand.Text()
	tmp, err := parent.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer parent.Remove(tmpName)
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return parent.Rename(tmpName, base)
}
