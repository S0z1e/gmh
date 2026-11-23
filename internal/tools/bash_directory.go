package tools

import (
	"context"
	"fmt"
	"os"

	"minimalharness/internal/workdir"
	"minimalharness/internal/workspace"
)

// BashDirectory records the path and directory identity presented for review.
type BashDirectory struct {
	identity workspace.Identity
	info     os.FileInfo
}

// Path returns the effective working directory to record in the invocation.
func (d *BashDirectory) Path() string { return d.identity.Path }

// PrepareBashDirectory resolves and validates the actual directory before review.
func (e *Executor) PrepareBashDirectory(cwd string) (*BashDirectory, error) {
	identity, err := workspace.Identify(workdir.Resolve(e.Root, cwd))
	if err != nil {
		return nil, fmt.Errorf("invalid cwd %q: %w", cwd, err)
	}
	info, err := e.validateBashDirectory(cwd, identity.Path)
	if err != nil {
		return nil, err
	}
	return &BashDirectory{identity: identity, info: info}, nil
}

type bashDirectoryKey struct{}

// WithBashDirectory carries the reviewed directory to the executor.
func WithBashDirectory(ctx context.Context, dir *BashDirectory) context.Context {
	return context.WithValue(ctx, bashDirectoryKey{}, dir)
}

func bashDirectoryFromContext(ctx context.Context) *BashDirectory {
	dir, _ := ctx.Value(bashDirectoryKey{}).(*BashDirectory)
	return dir
}

func (d *BashDirectory) check(dir string) error {
	changed := fmt.Errorf("bash working directory changed after it was prepared for review: %q", d.Path())
	if dir != d.Path() {
		return changed
	}
	current, err := workspace.Identify(dir)
	if err != nil || current != d.identity {
		return changed
	}
	if current.FileID == "" {
		// Some filesystems omit creation identity. Compare the OS file info
		// there. On Windows Identify obtains an eager ID from a handle.
		info, err := os.Stat(dir)
		if err != nil || !os.SameFile(d.info, info) {
			return changed
		}
	}
	return nil
}
