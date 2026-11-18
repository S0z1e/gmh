package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"minimalharness/internal/guidance"
)

const GuidanceTool = "read_guidance"

func (e *Executor) GuidanceEnabled() bool {
	return e != nil && !e.UntrustedWorkspace && e.Guidance != nil
}

// ResolveGuidance uses the path requested by the final invocation and checks
// that the snapshot applies to the same effective target as its pinned mutation.
func (m *Mutation) ResolveGuidance(ctx context.Context) (guidance.Snapshot, error) {
	if m == nil || !m.executor.GuidanceEnabled() {
		return guidance.Snapshot{}, &PolicyError{Reason: "project guidance is unavailable without a trusted workspace"}
	}
	r := m.executor.Guidance
	if err := m.target.CheckRootFileID(r.FileID()); err != nil {
		return guidance.Snapshot{}, err
	}
	snapshot, err := r.Resolve(ctx, m.Path)
	if err != nil {
		return guidance.Snapshot{}, err
	}
	if snapshot.Target != filepath.Join(snapshot.Root, m.TargetPath) {
		return guidance.Snapshot{}, fmt.Errorf("guidance target changed after preparing the mutation")
	}
	return snapshot, nil
}

func (e *Executor) readGuidance(ctx context.Context, args string) (ToolResult, error) {
	if !e.GuidanceEnabled() {
		return ToolResult{}, &PolicyError{Reason: "project guidance is unavailable without a trusted workspace"}
	}
	var a struct {
		Target string `json:"target"`
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Offset int    `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{}, &InputError{Err: fmt.Errorf("bad %s args: %w", GuidanceTool, err)}
	}
	limit := guidance.PrefixBytes
	if a.Limit != nil {
		limit = *a.Limit
	}
	chunk, err := e.Guidance.Read(ctx, a.Target, a.Path, a.SHA256, a.Offset, limit)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Text: fmt.Sprintf("[project guidance %q, SHA-256 %s, bytes [%d,%d) of %d, next=%d, more=%t]\n%s", chunk.Path, chunk.SHA256, chunk.Offset, chunk.Next, chunk.Bytes, chunk.Next, chunk.More, chunk.Text)}, nil
}
