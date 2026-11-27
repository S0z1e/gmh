package tools

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

const jobTokenEnvVar = "GMH_JOB_TOKEN"

var (
	inspectProcessErrFn        = inspectProcessErr
	stopVerifiedProcessGroupFn = stopVerifiedProcessGroupWithProof
)

// ErrProcessIdentityMismatch reports that an OS process ID no longer matches the
// recorded process group or OS start token (such as after PID reuse).
var ErrProcessIdentityMismatch = errors.New("process identity mismatch")

var errIncompleteGroupProof = errors.New("incomplete process group verification proof")

var errGroupMembersStillAlive = errors.New("verified process group members still alive after signalling")

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

type verifiedProcFD struct {
	fd         int
	startToken string
}

type processGroupProof struct {
	Leader            ProcessIdentityInfo
	UID               uint32
	HasUID            bool
	SID               int
	Sess              uintptr
	JobToken          string
	OutputFile        string
	LeaderPipeTargets []string
	KnownMembers      map[int]string
	VerifiedAt        time.Time
	verifiedFDs       map[int]verifiedProcFD
	jobHandle         uintptr
	ExitObservedAt    time.Time
}

func canRetryIncompleteGroupProof(proof processGroupProof) bool {
	return proof.JobToken == "" &&
		!proof.VerifiedAt.IsZero() &&
		!proof.ExitObservedAt.IsZero() &&
		proof.KnownMembers[proof.Leader.PID] == proof.Leader.StartToken
}

func cloneGroupProof(p processGroupProof) processGroupProof {
	if len(p.LeaderPipeTargets) > 0 {
		p.LeaderPipeTargets = append([]string(nil), p.LeaderPipeTargets...)
	} else {
		p.LeaderPipeTargets = nil
	}
	if len(p.KnownMembers) == 0 {
		p.KnownMembers = nil
		return p
	}
	cloned := make(map[int]string, len(p.KnownMembers))
	for k, v := range p.KnownMembers {
		cloned[k] = v
	}
	p.KnownMembers = cloned
	return p
}

func mergePipeTargets(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]struct{}, len(dst)+len(src))
	out := make([]string, 0, len(dst)+len(src))
	for _, s := range dst {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	for _, s := range src {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func mergeGroupMembers(dst, src map[int]string) map[int]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[int]string, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// ProcessIdentity returns the live OS process group and start token for pid when
// the process is alive and verifiable on this platform.
func ProcessIdentity(pid int) (pgid int, startToken string, alive bool) {
	info := inspectProcess(pid)
	return info.PGID, info.StartToken, info.Alive && !info.Zombie && info.PGID > 0 && info.StartToken != ""
}

func isFDLimitErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

func recordFirstMismatch(dst *error, err error) {
	if dst != nil && *dst == nil && err != nil {
		*dst = err
	}
}

func detachStaleVerifiedFDs(proof *processGroupProof) processGroupProof {
	if proof == nil || len(proof.verifiedFDs) == 0 || len(proof.KnownMembers) == 0 {
		return processGroupProof{}
	}
	var stale processGroupProof
	for pid, entry := range proof.verifiedFDs {
		if tok := proof.KnownMembers[pid]; tok != "" && entry.startToken != tok {
			if stale.verifiedFDs == nil {
				stale.verifiedFDs = make(map[int]verifiedProcFD)
			}
			stale.verifiedFDs[pid] = entry
			delete(proof.verifiedFDs, pid)
		}
	}
	return stale
}

func waitForKnownMembersExit(id ProcessIdentityInfo, signalErr error, checkLive func() (bool, error)) error {
	var lastLiveErr error
	for i := 0; i < 20; i++ {
		live, lerr := checkLive()
		if lerr != nil {
			lastLiveErr = lerr
		}
		if !live && lerr == nil {
			return signalErr
		}
		time.Sleep(5 * time.Millisecond)
	}
	if signalErr != nil {
		return signalErr
	}
	if lastLiveErr != nil {
		return lastLiveErr
	}
	return fmt.Errorf("verified process group members for pid %d still alive after signalling: %w", id.PID, errGroupMembersStillAlive)
}
