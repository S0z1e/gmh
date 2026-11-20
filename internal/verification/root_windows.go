package verification

import (
	"minimalharness/internal/workspace"
	"os"
)

func openSnapshotRoot(identity workspace.Identity) (*os.Root, error) {
	root, err := os.OpenRoot(identity.Path)
	if err != nil {
		return nil, err
	}
	if err := checkOpenedRoot(root, identity); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}
