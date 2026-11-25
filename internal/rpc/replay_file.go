package rpc

import (
	"fmt"
	"os"
)

func replayDirMode() os.FileMode { return 0o700 }

func checkReplayPath(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("rpc replay log must be a regular file without symlinks")
	}
	return nil
}
