package guidance

import (
	"context"
	"fmt"

	"minimalharness/internal/workspace"
)

// Resolver is constructed only for a workspace the caller has already trusted.
// Each operation verifies the directory identity against the opened handle.
type Resolver struct{ identity workspace.Identity }

func ForWorkspace(identity workspace.Identity) *Resolver {
	return &Resolver{identity: identity}
}

func (r *Resolver) FileID() string { return r.identity.FileID }

func (r *Resolver) Resolve(ctx context.Context, target string) (Snapshot, error) {
	if r.identity.FileID == "" {
		return Snapshot{}, fmt.Errorf("trusted workspace identity is unavailable")
	}
	return resolve(ctx, r.identity.Path, target, r.identity.FileID)
}

func (r *Resolver) Read(ctx context.Context, target, path, expectedHash string, offset, limit int) (Chunk, error) {
	if r.identity.FileID == "" {
		return Chunk{}, fmt.Errorf("trusted workspace identity is unavailable")
	}
	return read(ctx, r.identity.Path, target, path, expectedHash, r.identity.FileID, offset, limit)
}
