package verification

import (
	"fmt"
	"minimalharness/internal/workspace"
	"os"
)

func checkOpenedRoot(root *os.Root, identity workspace.Identity) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	id, err := workspace.DirectoryFileID(f)
	if err != nil {
		return err
	}
	if id == "" || id != identity.FileID {
		return fmt.Errorf("verification opened a different workspace directory")
	}
	return nil
}
