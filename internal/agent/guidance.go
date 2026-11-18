package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"minimalharness/internal/guidance"
	"minimalharness/internal/llm"
	"minimalharness/internal/textutil"
	"minimalharness/internal/tools"
)

const guidanceDeferredKind = "guidance_deferred"
const guidanceMessageKind = "project_guidance"
const guidanceChunkKind = "project_guidance_chunk"
const maxGuidancePreviewBytes = 32 << 10

// The ledger is local to the current prompt view. A new turn or checkpoint
// cannot claim guidance was shown in the current view merely from its history.
type guidanceTurn struct {
	resolver   *guidance.Resolver
	rootKey    string
	rootStatus string
	lastTarget string
	shown      map[string]guidanceView
	unread     *tools.GuidanceDelivery
}

type guidanceView struct {
	scopeKey string
	rootKey  string
	stale    bool
}

func newGuidanceTurn(e *tools.Executor) *guidanceTurn {
	if !e.GuidanceEnabled() {
		return nil
	}
	return &guidanceTurn{resolver: e.Guidance, shown: map[string]guidanceView{}}
}

func guidanceKey(files []guidance.File) string {
	var ids strings.Builder
	for _, f := range files {
		ids.WriteString(f.SHA256)
		ids.WriteString(f.Path)
		ids.WriteByte(0)
	}
	return factDigest(ids.String())
}

// The sources are ordered outermost-first. Everything before the first
// descendant scope is also part of the workspace-wide snapshot.
func guidanceViewFor(s guidance.Snapshot) guidanceView {
	count := len(s.Files)
	for i, f := range s.Files {
		rel, err := filepath.Rel(f.Scope, s.Root)
		if err != nil || !filepath.IsLocal(rel) {
			count = i
			break
		}
	}
	return guidanceView{scopeKey: guidanceKey(s.Files), rootKey: guidanceKey(s.Files[:count])}
}

func (g *guidanceTurn) changeRoot(status, key string) {
	for directory, view := range g.shown {
		if key == "" || view.rootKey != key {
			// The old scoped message is still in the prompt. Remember
			// that it must be superseded even if only root rules remain.
			view.stale = true
			g.shown[directory] = view
		}
	}
	g.rootStatus, g.rootKey = status, key
}

func renderGuidance(s guidance.Snapshot, scope string, remaining int) llm.Message {
	var b strings.Builder
	var offers []llm.GuidanceCursor
	fmt.Fprintf(&b, "[harness project guidance for directory %q]\nThis snapshot supersedes previous guidance for this directory. Apply files in the order shown; closer directories refine ancestors, and CLAUDE.md follows AGENTS.md in the same directory.\nTarget available for read_guidance: %q.\n", scope, s.Target)
	if len(s.Files) == 0 {
		b.WriteString("There are no applicable guidance files for this target.")
	}
	for _, f := range s.Files {
		preview := textutil.Prefix(f.Prefix, remaining)
		remaining -= len(preview)
		fmt.Fprintf(&b, "\nSource %q, SHA-256 %s, total bytes %d:\n%s\n", f.Path, f.SHA256, f.Bytes, preview)
		if len(preview) < f.Bytes {
			offers = append(offers, llm.GuidanceCursor{Target: s.Target, Path: f.Path, SHA256: f.SHA256, Offset: len(preview)})
			fmt.Fprintf(&b, "[Source continues; use read_guidance with target %q, path %q, sha256 %q and offset %d when the remainder is needed.]\n", s.Target, f.Path, f.SHA256, len(preview))
		}
	}
	return llm.Message{Role: "user", Kind: guidanceMessageKind, Content: b.String(), GuidanceOffers: offers}
}

// WithoutHistoricalGuidance returns a prompt or hook view without earlier project text.
// Project instructions apply to one turn's checked worktree. Keep their source
// records in the transcript, but rebuild the live instructions each turn, even
// when resuming a session that was explicitly rebound to another workspace.
func WithoutHistoricalGuidance(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Kind {
		case guidanceMessageKind:
			continue
		case guidanceChunkKind:
			if m.Role != "tool" {
				continue
			}
			m.Content = "[Earlier project guidance chunk omitted; discover current guidance for the next target.]"
			m.GuidanceOffers = nil
		}
		out = append(out, m)
	}
	return out
}

func (g *guidanceTurn) resolveRoot(ctx context.Context) (guidance.Snapshot, error) {
	snapshot, err := g.resolver.Resolve(ctx, "")
	if err != nil {
		if status := "error:" + err.Error(); status != g.rootStatus {
			g.changeRoot(status, "")
		}
	} else if key := guidanceKey(snapshot.Files); key != g.rootStatus {
		g.changeRoot(key, key)
	}
	return snapshot, err
}

func rootGuidance(snapshot guidance.Snapshot, err error, preview int) llm.Message {
	if err != nil {
		return llm.Message{Role: "user", Kind: guidanceMessageKind, Content: "[harness project guidance] Workspace guidance is unavailable: " + err.Error()}
	}
	return renderGuidance(snapshot, snapshot.Root, preview)
}

func (g *guidanceTurn) refreshRoot(ctx context.Context, c *Config, msgs []llm.Message) []llm.Message {
	if ctx.Err() != nil {
		return msgs
	}
	previous := g.rootStatus
	snapshot, err := g.resolveRoot(ctx)
	if previous == g.rootStatus || ctx.Err() != nil || previous == "" && err == nil && len(snapshot.Files) == 0 {
		return msgs
	}
	msg := rootGuidance(snapshot, err, maxGuidancePreviewBytes)
	c.persist(msg)
	return append(msgs, msg)
}

func (g *guidanceTurn) forMutation(ctx context.Context, draft *tools.Mutation) (*llm.Message, error) {
	snapshot, err := draft.ResolveGuidance(ctx)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(snapshot.Target)
	view := guidanceViewFor(snapshot)
	g.lastTarget = snapshot.Target
	shown, ok := g.shown[dir]
	if (ok && shown == view) || (!ok && view.scopeKey == g.rootKey) {
		return nil, nil
	}
	if dir == snapshot.Root {
		g.changeRoot(view.rootKey, view.rootKey)
	} else {
		g.shown[dir] = view
	}
	msg := renderGuidance(snapshot, dir, maxGuidancePreviewBytes)
	return &msg, nil
}

// Only messages included in this request grant offsets. A tool in the same
// batch as a successful read cannot use its next offset before seeing it.
func guidanceOffers(msgs []llm.Message) map[llm.GuidanceCursor]bool {
	offers := make(map[llm.GuidanceCursor]bool)
	for _, m := range msgs {
		for _, cursor := range m.GuidanceOffers {
			offers[cursor] = true
		}
	}
	return offers
}

func attachGuidanceDelivery(m *llm.Message, delivery tools.GuidanceDelivery) {
	m.Kind = guidanceChunkKind
	if next, more := delivery.Next(); more {
		m.GuidanceOffers = []llm.GuidanceCursor{next}
	}
}

// prepareCheckpoint rebuilds the live instructions before any durable rewrite.
// It also carries the latest tool result the model has not seen yet, after
// checking it again against the current source and target.
func (g *guidanceTurn) prepareCheckpoint(ctx context.Context, msgs []llm.Message, fits func([]llm.Message) bool) []llm.Message {
	g.rootKey, g.rootStatus = "", ""
	clear(g.shown)
	root, rootErr := g.resolveRoot(ctx)
	var scope guidance.Snapshot
	if g.lastTarget != "" && ctx.Err() == nil {
		if snapshot, err := g.resolver.Resolve(ctx, g.lastTarget); err == nil && snapshot.Target == g.lastTarget {
			dir := filepath.Dir(snapshot.Target)
			if view := guidanceViewFor(snapshot); dir != snapshot.Root && view.scopeKey != g.rootKey {
				scope = snapshot
				g.shown[dir] = view
			}
		}
	}
	var delivery *tools.GuidanceDelivery
	if unread := g.unread; unread != nil && ctx.Err() == nil {
		cursor := unread.Cursor
		if chunk, err := g.resolver.Read(ctx, cursor.Target, cursor.Path, cursor.SHA256, cursor.Offset, unread.Limit); err == nil {
			fresh := *unread
			fresh.Chunk = chunk
			delivery = &fresh
		}
	}
	base := WithoutHistoricalGuidance(msgs)
	build := func(preview, chunkBytes int) []llm.Message {
		candidate := append([]llm.Message(nil), base...)
		if rootErr != nil || len(root.Files) > 0 {
			candidate = append(candidate, rootGuidance(root, rootErr, preview))
		}
		if scope.Target != "" {
			candidate = append(candidate, renderGuidance(scope, filepath.Dir(scope.Target), preview))
		}
		if delivery != nil {
			current := *delivery
			current.Chunk.Text = textutil.Prefix(current.Chunk.Text, chunkBytes)
			current.Chunk.Next = current.Chunk.Offset + len(current.Chunk.Text)
			current.Chunk.More = current.Chunk.Next < current.Chunk.Bytes
			// Summary compaction may have dropped the original tool call.
			m := llm.Message{Role: "user", Content: current.Text()}
			attachGuidanceDelivery(&m, current)
			candidate = append(candidate, m)
		}
		return candidate
	}
	chunkBytes := 0
	if delivery != nil {
		chunkBytes = len(delivery.Chunk.Text)
	}
	candidate := build(maxGuidancePreviewBytes, chunkBytes)
	if fits(candidate) {
		return candidate
	}
	// Keep unread bytes first. Even with no room for source text, include
	// its digest and exact next offset so the model can ask for it.
	for preview := maxGuidancePreviewBytes / 2; ; preview /= 2 {
		candidate = build(preview, chunkBytes)
		if fits(candidate) || preview == 0 {
			break
		}
	}
	for chunkBytes > 0 && !fits(candidate) {
		chunkBytes /= 2
		candidate = build(0, chunkBytes)
	}
	return candidate // the checkpoint's final budget check rejects even metadata if necessary
}

func (c *Config) deferForGuidance(msgs []llm.Message, calls []llm.ToolCall, invocation string, update *llm.Message, cause error) []llm.Message {
	if update != nil {
		c.DoomLoop().clearStreak()
	}
	for i, call := range calls {
		obs := "Not executed: reconsider this operation after reviewing the updated project guidance. Earlier calls in the batch may already have run."
		if cause != nil {
			obs = "Not executed: could not confirm current project guidance: " + cause.Error()
		}
		id := ""
		if i == 0 {
			id = invocation
		}
		if id == "" {
			c.emit(Event{Kind: EventToolStart, ToolName: call.Function.Name, ToolArgs: call.Function.Arguments, ToolCallID: call.ID})
		}
		c.emit(Event{Kind: EventToolEnd, ToolName: call.Function.Name, ToolCallID: call.ID, InvocationID: id, ToolOutput: obs, ToolStatus: tools.ResultDeferred})
		result := llm.Message{Role: "tool", Kind: guidanceDeferredKind, ToolCallID: call.ID, InvocationID: id, Content: obs}
		c.persist(result)
		msgs = append(msgs, result)
	}
	if update != nil {
		c.persist(*update)
		msgs = append(msgs, *update)
	}
	return msgs
}
