package agent

import (
	"context"
	"errors"
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
	shown      map[string]string
}

func newGuidanceTurn(e *tools.Executor) *guidanceTurn {
	if !e.GuidanceEnabled() {
		return nil
	}
	return &guidanceTurn{resolver: e.Guidance, shown: map[string]string{}}
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

func renderGuidance(s guidance.Snapshot, scope string) llm.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "[harness project guidance for directory %q]\nThis snapshot supersedes previous guidance for this directory. Apply files in the order shown; closer directories refine ancestors, and CLAUDE.md follows AGENTS.md in the same directory.\nTarget available for read_guidance: %q.\n", scope, s.Target)
	if len(s.Files) == 0 {
		b.WriteString("There are no applicable guidance files for this target.")
	}
	remaining := maxGuidancePreviewBytes
	for _, f := range s.Files {
		preview := textutil.Prefix(f.Prefix, remaining)
		remaining -= len(preview)
		fmt.Fprintf(&b, "\nSource %q, SHA-256 %s, total bytes %d:\n%s\n", f.Path, f.SHA256, f.Bytes, preview)
		if len(preview) < f.Bytes {
			fmt.Fprintf(&b, "[Source continues; use read_guidance with target %q, path %q, sha256 %q and offset %d when the remainder is needed.]\n", s.Target, f.Path, f.SHA256, len(preview))
		}
	}
	return llm.Message{Role: "user", Kind: guidanceMessageKind, Content: b.String()}
}

// Project instructions apply to one turn's checked worktree. Keep their source
// records in the transcript, but rebuild the live instructions each turn, even
// when resuming a session that was explicitly rebound to another workspace.
func retireProjectGuidance(msgs []llm.Message) []llm.Message {
	out := msgs[:0]
	for _, m := range msgs {
		switch m.Kind {
		case guidanceMessageKind:
			continue
		case guidanceChunkKind:
			m.Content = "[Earlier project guidance chunk omitted; discover current guidance for the next target.]"
		}
		out = append(out, m)
	}
	return out
}

func (g *guidanceTurn) refreshRoot(ctx context.Context, c *Config, msgs []llm.Message) []llm.Message {
	if ctx.Err() != nil {
		return msgs
	}
	snapshot, err := g.resolver.Resolve(ctx, "")
	var msg llm.Message
	var status string
	if err != nil {
		status = "error:" + err.Error()
		g.rootKey = ""
		msg = llm.Message{Role: "user", Kind: guidanceMessageKind, Content: "[harness project guidance] Workspace guidance is unavailable: " + err.Error()}
	} else {
		g.rootKey = guidanceKey(snapshot.Files)
		status = g.rootKey
		msg = renderGuidance(snapshot, snapshot.Root)
	}
	if status == g.rootStatus || ctx.Err() != nil {
		return msgs
	}
	if g.rootStatus == "" && err == nil && len(snapshot.Files) == 0 {
		g.rootStatus = status
		return msgs
	}
	g.rootStatus = status
	c.persist(msg)
	return append(msgs, msg)
}

func (g *guidanceTurn) forMutation(ctx context.Context, draft *tools.Mutation) (*llm.Message, error) {
	snapshot, err := draft.ResolveGuidance(ctx)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(snapshot.Target)
	key := guidanceKey(snapshot.Files)
	g.lastTarget = snapshot.Target
	shown, ok := g.shown[dir]
	if (ok && shown == key) || (!ok && key == g.rootKey) {
		return nil, nil
	}
	if dir == snapshot.Root {
		g.rootKey, g.rootStatus = key, key
	} else {
		g.shown[dir] = key
	}
	msg := renderGuidance(snapshot, dir)
	return &msg, nil
}

// Preserve the latest mutation's live scope across a checkpoint. Otherwise a
// deferral that triggers compaction can repeatedly lose its own instructions
// before the model gets a chance to see them and revise the call.
func (g *guidanceTurn) afterCompact(ctx context.Context, c *Config, msgs []llm.Message) []llm.Message {
	g.rootKey, g.rootStatus = "", ""
	clear(g.shown)
	msgs = g.refreshRoot(ctx, c, retireProjectGuidance(msgs))
	if g.lastTarget == "" || ctx.Err() != nil {
		return msgs
	}
	snapshot, err := g.resolver.Resolve(ctx, g.lastTarget)
	if err != nil || snapshot.Target != g.lastTarget {
		return msgs // an actual mutation checks again and reports any resolution error
	}
	dir := filepath.Dir(snapshot.Target)
	key := guidanceKey(snapshot.Files)
	if dir == snapshot.Root || key == g.rootKey {
		return msgs
	}
	g.shown[dir] = key
	msg := renderGuidance(snapshot, dir)
	c.persist(msg)
	return append(msgs, msg)
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
		c.emit(Event{Kind: EventToolEnd, ToolName: call.Function.Name, ToolCallID: call.ID, InvocationID: id, ToolOutput: obs, ToolErr: errors.New(obs)})
		result := llm.Message{Role: "tool", Kind: guidanceDeferredKind, ToolCallID: call.ID, InvocationID: id, Content: obs, ToolError: true}
		c.persist(result)
		msgs = append(msgs, result)
	}
	if update != nil {
		c.persist(*update)
		msgs = append(msgs, *update)
	}
	return msgs
}
