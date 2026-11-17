//go:build !windows

package verification

import (
	"fmt"
	"os"
	"strconv"

	"minimalharness/internal/workspace"
)

func openSnapshotRoot(identity workspace.Identity) (*os.Root, error) {
	// OpenRoot itself opens read-only before checking directory type on Unix;
	// opening a substituted FIFO there can block forever. Pin with O_NONBLOCK,
	// validate the incarnation, then duplicate the directory via its fd alias.
	f, err := os.OpenFile(identity.Path, os.O_RDONLY|nonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	id, err := workspace.DirectoryFileID(f)
	if err != nil {
		return nil, err
	}
	if id == "" || id != identity.FileID {
		return nil, fmt.Errorf("verification workspace directory changed before opening")
	}
	root, err := os.OpenRoot("/dev/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
	if err != nil {
		return nil, err
	}
	if err := checkOpenedRoot(root, identity); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}
