package tools

import (
	"crypto/rand"
	"os"

	"minimalharness/internal/fileguard"
)

// atomicWriteFile prepares and pins a target for callers without an approval lease.
func atomicWriteFile(r *os.Root, name string, data []byte) error {
	target, err := fileguard.ResolveTarget(r, name)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := target.Check(); err != nil {
		return err
	}
	return atomicWriteTarget(target, data)
}

func atomicWriteTarget(target *fileguard.Target, data []byte) error {
	if target.Info == nil {
		return os.ErrNotExist
	}
	parent, base := target.Parent, target.Name
	mode := target.Info.Mode().Perm()
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
