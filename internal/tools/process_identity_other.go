//go:build !linux && !darwin && !windows

package tools

import "context"

func processGroupID(pid int) int { return pid }

func inspectProcess(pid int) processInfo { return processInfo{} }

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	return ErrProcessIdentityMismatch
}

func stopAdoptedProcessGroupAfterExit(id ProcessIdentityInfo) error {
	return ErrProcessIdentityMismatch
}

func waitAdoptedProcess(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
	return -1, false, ErrProcessIdentityMismatch
}
