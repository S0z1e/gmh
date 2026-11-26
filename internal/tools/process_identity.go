package tools

import (
	"errors"
	"time"
)

// ErrProcessIdentityMismatch reports that an OS process ID no longer matches the
// recorded process group or OS start token (such as after PID reuse).
var ErrProcessIdentityMismatch = errors.New("process identity mismatch")

var errAdoptedProcessExited = errors.New("adopted background job exited after harness restart; exit status unavailable")

// ProcessIdentityInfo captures the immutable OS process identity recorded for a
// background command so reopen can verify it before adopting or signalling.
type ProcessIdentityInfo struct {
	PID        int    `json:"pid,omitempty"`
	PGID       int    `json:"pgid,omitempty"`
	StartToken string `json:"process_started,omitempty"`
}

// Valid reports whether all identity fields needed for safe OS verification
// are populated.
func (p ProcessIdentityInfo) Valid() bool {
	return p.PID > 0 && p.PGID > 0 && p.StartToken != ""
}

// Matches reports whether live OS process metadata info still represents this
// exact recorded non-zombie process and group.
func (p ProcessIdentityInfo) Matches(info processInfo) bool {
	return p.Valid() && info.Alive && !info.Zombie && info.StartToken == p.StartToken && info.PGID == p.PGID
}

type processInfo struct {
	Alive      bool
	Zombie     bool
	PGID       int
	StartToken string
	UID        uint32
	HasUID     bool
	SID        int
	Sess       uintptr
}

type processGroupProof struct {
	Leader         ProcessIdentityInfo
	UID            uint32
	HasUID         bool
	SID            int
	Sess           uintptr
	ExitObservedAt time.Time
}

// ProcessIdentity returns the live OS process group and start token for pid when
// the process is alive and verifiable on this platform.
func ProcessIdentity(pid int) (pgid int, startToken string, alive bool) {
	info := inspectProcess(pid)
	return info.PGID, info.StartToken, info.Alive && !info.Zombie && info.PGID > 0 && info.StartToken != ""
}
