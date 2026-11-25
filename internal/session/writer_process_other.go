//go:build !linux && !darwin && !windows

package session

import "fmt"

func writerProcessIdentity(int) (string, error) { return "", nil }

// writerOwnership cannot prove anything here, so a steal must refuse.
func writerOwnership(LockOwner) (alive, proven bool, err error) {
	return false, false, nil
}

func terminateWriterProcess(LockOwner) error {
	return fmt.Errorf("terminating an active session writer is unsupported on this platform")
}
