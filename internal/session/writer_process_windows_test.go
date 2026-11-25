//go:build windows

package session

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestWindowsWriterIdentityMatchesSelfAndRefusesStrangers(t *testing.T) {
	self, err := writerProcessIdentity(os.Getpid())
	if err != nil || !strings.HasPrefix(self, writerIdentityPrefix) || !strings.Contains(self, ":") {
		t.Fatalf("self identity = %q err %v", self, err)
	}
	alive, proven, err := writerOwnership(LockOwner{PID: os.Getpid(), ProcessStarted: self})
	if err != nil || !alive || !proven {
		t.Fatalf("self ownership = alive %v proven %v err %v", alive, proven, err)
	}
	alive, proven, err = writerOwnership(LockOwner{PID: os.Getpid(), ProcessStarted: self + "-tampered"})
	if err != nil || alive || !proven {
		t.Fatalf("tampered identity = alive %v proven %v err %v", alive, proven, err)
	}
	// OpenProcess returns ERROR_INVALID_PARAMETER for a PID that does not exist.
	// That owner is dead, so the steal may take the lock. A query error is the
	// case that stays unproven.
	alive, proven, err = writerOwnership(LockOwner{PID: 1<<30 - 1, ProcessStarted: "win:1:missing"})
	if err != nil || alive || !proven {
		t.Fatalf("missing pid = alive %v proven %v err %v", alive, proven, err)
	}
	alive, proven, err = writerOwnership(LockOwner{PID: os.Getpid(), ProcessStarted: ""})
	if err != nil || alive || proven {
		t.Fatalf("blank identity = alive %v proven %v err %v", alive, proven, err)
	}
	if err := terminateWriterProcess(LockOwner{PID: os.Getpid(), ProcessStarted: "win:0:not-this-process"}); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("terminate with a mismatched identity = %v", err)
	}
	if err := terminateWriterProcess(LockOwner{PID: 1<<30 - 1, ProcessStarted: "win:1:missing"}); err != nil && !errors.Is(err, errWriterExited) {
		t.Fatalf("terminate missing pid = %v", err)
	}
}
