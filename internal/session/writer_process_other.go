//go:build !linux && !darwin

package session

import "fmt"

func writerProcessIdentity(int) string { return "" }

func terminateWriterProcess(LockOwner) error {
	return fmt.Errorf("terminating an active session writer is unsupported on this platform")
}
