package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"minimalharness/internal/guidance"
	"minimalharness/internal/llm"
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

type guidanceReadArgs struct {
	Target string `json:"target"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
}

type GuidanceDelivery struct {
	Cursor llm.GuidanceCursor
	Limit  int
	Chunk  guidance.Chunk
}

func (d GuidanceDelivery) Text() string {
	ch := d.Chunk
	return fmt.Sprintf("[project guidance %q for target %q, SHA-256 %s, bytes [%d,%d) of %d, next=%d, more=%t]\n%s", ch.Path, d.Cursor.Target, ch.SHA256, ch.Offset, ch.Next, ch.Bytes, ch.Next, ch.More, ch.Text)
}

func (d GuidanceDelivery) Next() (llm.GuidanceCursor, bool) {
	next := d.Cursor
	next.Offset = d.Chunk.Next
	return next, d.Chunk.More
}

type guidancePermissionKey struct{}

// WithGuidanceOffers grants only exact offsets shown in this model request.
func WithGuidanceOffers(ctx context.Context, allowed func(llm.GuidanceCursor) bool) context.Context {
	return context.WithValue(ctx, guidancePermissionKey{}, allowed)
}

func (e *Executor) readGuidance(ctx context.Context, args string) (ToolResult, error) {
	if !e.GuidanceEnabled() {
		return ToolResult{}, &PolicyError{Reason: "project guidance is unavailable without a trusted workspace"}
	}
	var a guidanceReadArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{}, &InputError{Err: fmt.Errorf("bad %s args: %w", GuidanceTool, err)}
	}
	allowed, _ := ctx.Value(guidancePermissionKey{}).(func(llm.GuidanceCursor) bool)
	if a.Offset == nil {
		return ToolResult{}, &InputError{Err: fmt.Errorf("guidance continuation requires a byte offset")}
	}
	cursor := llm.GuidanceCursor{Target: a.Target, Path: a.Path, SHA256: a.SHA256, Offset: *a.Offset}
	if allowed == nil || !allowed(cursor) {
		return ToolResult{}, &InputError{Err: fmt.Errorf("guidance continuation must use an advertised target, path, digest and offset")}
	}
	limit := guidance.PrefixBytes
	if a.Limit != nil {
		limit = *a.Limit
	}
	chunk, err := e.Guidance.Read(ctx, a.Target, a.Path, a.SHA256, cursor.Offset, limit)
	if err != nil {
		return ToolResult{}, err
	}
	delivery := GuidanceDelivery{Cursor: cursor, Limit: limit, Chunk: chunk}
	return ToolResult{Text: delivery.Text(), Guidance: &delivery}, nil
}
