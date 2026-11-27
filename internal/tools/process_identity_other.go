//go:build !linux && !darwin && !windows

package tools

import "context"

func processGroupID(pid int) int { return pid }

func inspectProcess(pid int) processInfo { return processInfo{} }

func inspectProcessErr(pid int) (processInfo, error) { return processInfo{}, nil }

func stopVerifiedProcessGroup(id ProcessIdentityInfo) error {
	return ErrProcessIdentityMismatch
}

func stopVerifiedProcessGroupWithProof(proof processGroupProof) error {
	return ErrProcessIdentityMismatch
}

func captureVerifiedGroupMembers(proof processGroupProof) map[int]string {
	return nil
}

func captureVerifiedGroupProof(proof *processGroupProof) {}

func closeVerifiedGroupProof(proof *processGroupProof) {}

func stopAdoptedProcessGroupAfterExit(proof processGroupProof) error {
	return ErrProcessIdentityMismatch
}

func stopAdoptedProcessGroupAfterExitTrack(proof *processGroupProof) error {
	return ErrProcessIdentityMismatch
}

func hasVerifiedLiveGroupMember(proof processGroupProof) bool { return false }

func waitAdoptedProcess(ctx context.Context, id ProcessIdentityInfo) (int, bool, error) {
	return -1, false, ErrProcessIdentityMismatch
}
