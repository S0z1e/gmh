package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/guidance"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
	"minimalharness/internal/workspace"
)

func guidanceFixture(t *testing.T) *Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	i, err := workspace.Identify(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Config{Exec: &tools.Executor{Root: i.Path, Guidance: guidance.ForWorkspace(i)}, Tools: ToolDefs(), Mode: "auto", MaxSteps: 10}
}

func putGuidanceFixture(t *testing.T, root, path, text string) string {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGuidanceFollowsEffectiveTargetAndDefersBatch(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	putGuidanceFixture(t, root, "AGENTS.md", "root-guide-marker")
	putGuidanceFixture(t, root, "pkg/AGENTS.md", "package-guide-marker")
	putGuidanceFixture(t, root, "sibling/AGENTS.md", "sibling-guide-marker")
	cfg.Exec.Plugins = &fakePluginRunner{rewrite: map[string]string{"write": `{"path":"pkg/new.go","content":"made"}`}}
	deferred := 0
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && strings.HasPrefix(e.ToolOutput, "Not executed:") {
			deferred++
			if e.ToolStatus != tools.ResultDeferred || e.ToolErr != nil {
				t.Errorf("skipped call %q reported status %q with error %v", e.ToolCallID, e.ToolStatus, e.ToolErr)
			}
			if _, err := os.Stat(filepath.Join(root, "pkg/new.go")); !os.IsNotExist(err) {
				t.Errorf("target existed during deferral: %v", err)
			}
		}
	}
	first := budgetCall("first", "write", `{"path":"sibling/new.go","content":"wrong"}`)
	suffix := budgetCall("suffix", "read", `{"path":"sibling/AGENTS.md"}`)
	retry := budgetCall("retry", "write", first.Function.Arguments)
	_, requests, err := runVerification(t, cfg, budgetSSE("", []llm.ToolCall{first, suffix}, nil, true), budgetSSE("", []llm.ToolCall{retry}, nil, true))
	if err != nil || deferred != 2 || len(requests) != 3 {
		t.Fatalf("requests=%d deferred=%d err=%v", len(requests), deferred, err)
	}
	if strings.Contains(requests[0], "package-guide-marker") || !strings.Contains(requests[1], "package-guide-marker") || strings.Contains(requests[1], "sibling-guide-marker") || strings.Contains(requests[1], "outcome is unknown") {
		t.Fatalf("incorrect effective guidance after deferral: %s", requests[1])
	}
	got, err := os.ReadFile(filepath.Join(root, "pkg/new.go"))
	if err != nil || string(got) != "made" {
		t.Fatalf("written bytes = %q, err = %v", got, err)
	}
}

func TestGuidanceRefreshesAfterApproval(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	putGuidanceFixture(t, root, "AGENTS.md", "approved-against-old-guide")
	cfg.Mode = "suggest"
	approved := 0
	cfg.Ask = func(ApprovalRequest) (bool, error) {
		approved++
		if _, err := os.Stat(filepath.Join(root, "new.go")); !os.IsNotExist(err) {
			t.Errorf("target was written before its current guidance was approved: %v", err)
		}
		if approved == 1 {
			putGuidanceFixture(t, root, "AGENTS.md", "approval-wait-updated-guide")
		}
		return true, nil
	}
	_, requests, err := runVerification(t, cfg, verificationWrite("new.go", "old proposal"), verificationWrite("new.go", "updated proposal"))
	if err != nil || approved != 2 || len(requests) != 3 {
		t.Fatalf("requests=%d approvals=%d err=%v", len(requests), approved, err)
	}
	if !strings.Contains(requests[1], "approval-wait-updated-guide") || !strings.Contains(requests[1], "Not executed:") {
		t.Fatalf("provider did not see revised guidance and deferral: %s", requests[1])
	}
	if got, err := os.ReadFile(filepath.Join(root, "new.go")); err != nil || string(got) != "updated proposal" {
		t.Fatalf("written bytes = %q, err = %v", got, err)
	}
}

func TestGuidanceContinuationDoesNotLoadUntrustedSources(t *testing.T) {
	cfg := guidanceFixture(t)
	full := strings.Repeat("é", 4500) + "tail-guide-marker"
	putGuidanceFixture(t, cfg.Exec.Root, "AGENTS.md", full)
	s, err := cfg.Exec.Guidance.Resolve(context.Background(), "")
	if err != nil || len(s.Files) != 1 || !s.Files[0].More {
		t.Fatalf("snapshot files=%d err=%v", len(s.Files), err)
	}
	f := s.Files[0]
	args, err := json.Marshal(map[string]any{"target": s.Target, "path": f.Path, "sha256": f.SHA256, "offset": len(f.Prefix)})
	if err != nil {
		t.Fatal(err)
	}
	_, requests, err := runVerification(t, cfg, toolCallSSE("next", tools.GuidanceTool, string(args)))
	if err != nil || len(requests) != 2 || strings.Contains(requests[0], "tail-guide-marker") || !strings.Contains(requests[1], "tail-guide-marker") {
		t.Fatalf("trusted continuation: requests=%d err=%v", len(requests), err)
	}
	cfg.Exec.UntrustedWorkspace = true
	_, requests, err = runVerification(t, cfg, toolCallSSE("next", tools.GuidanceTool, string(args)))
	if err != nil || len(requests) != 2 || strings.Contains(requests[1], "tail-guide-marker") || !strings.Contains(requests[1], "unavailable without a trusted workspace") {
		t.Fatalf("untrusted continuation: requests=%d err=%v", len(requests), err)
	}
}

func TestGuidanceSwitchesTargetsAndDetectsRemovedScope(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	putGuidanceFixture(t, root, "AGENTS.md", "workspace-guidance")
	pathA := putGuidanceFixture(t, root, "a/AGENTS.md", "package-a-guidance")
	putGuidanceFixture(t, root, "b/AGENTS.md", "package-b-guidance")
	deferred := 0
	cfg.OnEvent = func(e Event) {
		if e.Kind != EventToolEnd || e.ToolName != "write" {
			return
		}
		if strings.HasPrefix(e.ToolOutput, "Not executed:") {
			deferred++
		}
		if e.ToolCallID == "run-b" {
			if err := os.Remove(pathA); err != nil {
				t.Error(err)
			}
		}
	}
	write := func(id, path string) string {
		args, _ := json.Marshal(map[string]string{"path": path, "content": id})
		return toolCallSSE(id, "write", string(args))
	}
	_, requests, err := runVerification(t, cfg, write("learn-a", "a/a.go"), write("run-a", "a/a.go"), write("learn-b", "b/b.go"), write("run-b", "b/b.go"), write("removed-a", "a/c.go"), write("retry-a", "a/c.go"))
	if err != nil || deferred != 3 || len(requests) != 7 {
		t.Fatalf("requests=%d deferred=%d err=%v", len(requests), deferred, err)
	}
	for _, want := range []struct {
		request int
		marker  string
	}{{1, "package-a-guidance"}, {3, "package-b-guidance"}, {5, "workspace-guidance"}} {
		index := strings.LastIndex(requests[want.request], "[harness project guidance")
		if index < 0 {
			t.Fatalf("request %d did not contain live guidance", want.request)
		}
		guide := requests[want.request][index:]
		if !strings.Contains(guide, want.marker) || want.request == 5 && strings.Contains(guide, "package-a-guidance") {
			t.Fatalf("request %d had incorrect live guidance: %s", want.request, guide)
		}
	}
	for name, want := range map[string]string{"a/a.go": "run-a", "b/b.go": "run-b", "a/c.go": "retry-a"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, err = %v; want %q", name, got, err, want)
		}
	}
}

func TestGuidanceRejectsReplacementWorkspace(t *testing.T) {
	cfg := guidanceFixture(t)
	original := cfg.Exec.Root
	putGuidanceFixture(t, original, "AGENTS.md", "original-guide")
	snapshot, err := cfg.Exec.Guidance.Resolve(context.Background(), "")
	if err != nil || len(snapshot.Files) != 1 {
		t.Fatalf("snapshot files=%d err=%v", len(snapshot.Files), err)
	}
	if err := os.Rename(original, filepath.Join(filepath.Dir(original), "moved")); err != nil {
		t.Fatal(err)
	}
	putGuidanceFixture(t, original, "AGENTS.md", "replacement-guide")
	f := snapshot.Files[0]
	read := verificationTool(tools.GuidanceTool, map[string]any{"target": snapshot.Target, "path": f.Path, "sha256": f.SHA256, "offset": 0})
	_, requests, err := runVerification(t, cfg, read, verificationWrite("new.go", "should not write"))
	if err != nil || len(requests) != 3 {
		t.Fatalf("requests=%d err=%v", len(requests), err)
	}
	for _, request := range requests {
		if strings.Contains(request, "original-guide") || strings.Contains(request, "replacement-guide") {
			t.Fatalf("unauthorized guidance reached the model: %s", request)
		}
	}
	if !strings.Contains(requests[2], "workspace directory identity changed") || !strings.Contains(requests[2], "Not executed:") {
		t.Fatalf("reason for refusing guidance and write missing: %s", requests[2])
	}
	if _, err := os.Stat(filepath.Join(original, "new.go")); !os.IsNotExist(err) {
		t.Fatalf("replacement workspace was written: %v", err)
	}
}

func TestGuidanceDeferralSurvivesSessionReplayAndCompaction(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	putGuidanceFixture(t, root, "pkg/AGENTS.md", "do-not-promote-this-guidance-to-history")
	opts := session.OpenOptions{Workspace: root}
	store, err := session.Open("guidance-replay", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Append(llm.Message{Role: "user", Kind: "task", Content: "Keep this original task"}); err != nil {
		t.Fatal(err)
	}
	cfg.Session = store
	write := budgetCall("learn", "write", `{"path":"pkg/new.go","content":"unused"}`)
	suffix := budgetCall("skip", "read", `{"path":"missing"}`)
	_, requests, err := runVerification(t, cfg, budgetSSE("", []llm.ToolCall{write, suffix}, nil, true))
	if err != nil || len(requests) != 2 {
		t.Fatalf("requests=%d err=%v", len(requests), err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.Open("guidance-replay", opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cfg.Session = reopened
	loaded, err := session.Load(reopened.ID)
	if err != nil {
		t.Fatal(err)
	}
	deferred := 0
	for _, m := range session.RepairToolBatches(loaded) {
		if m.Kind == "interrupted" {
			t.Fatal("replay synthesized an interrupted tool result for a settled deferral")
		}
		if m.Kind == guidanceDeferredKind {
			deferred++
			if m.Observation != nil || m.InvocationID != "" {
				t.Fatalf("nonexecution has dispatch evidence: %+v", m)
			}
		}
	}
	if deferred != 2 {
		t.Fatalf("settled calls = %d, want two", deferred)
	}
	cfg.CompactHistory = func(_ context.Context, input []llm.Message) (string, llm.Usage, error) {
		for _, m := range input {
			if strings.Contains(m.Content, "do-not-promote-this-guidance-to-history") {
				t.Error("original project guidance reached the summarizer")
			}
		}
		return "The write was deferred", llm.Usage{}, nil
	}
	out, _, err := cfg.Compact(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := cfg.compactState(out)
	if err != nil || len(state.Pending) != 0 || len(state.Failures) != 0 || state.ActiveTask == nil || state.ActiveTask.Content != "Keep this original task" {
		t.Fatalf("deferred batch polluted checkpoint: %+v, %v", state, err)
	}
}

func TestGuidanceThatTriggeredCompactionIsShownBeforeRetry(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	guide := putGuidanceFixture(t, root, "pkg/AGENTS.md", strings.Repeat("initial-", 900))
	cfg.ContextLimit = 60000
	cfg.CompactHistory = func(_ context.Context, _ []llm.Message) (string, llm.Usage, error) {
		cfg.ContextLimit = 60000
		if err := os.WriteFile(guide, []byte("fresh-rules-after-compaction"), 0600); err != nil {
			t.Fatal(err)
		}
		return "The write was deferred until the model sees the target scope", llm.Usage{}, nil
	}
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "learn" && strings.HasPrefix(e.ToolOutput, "Not executed:") {
			cfg.ContextLimit = 2350
		}
	}
	write := func(id string) string {
		return toolCallSSE(id, "write", `{"path":"pkg/new.go","content":"applied after compaction"}`)
	}
	_, requests, err := runVerification(t, cfg, write("learn"), write("retry"))
	if err != nil || len(requests) != 3 {
		t.Fatalf("requests=%d err=%v", len(requests), err)
	}
	if !strings.Contains(requests[1], "fresh-rules-after-compaction") || strings.Contains(requests[1], "initial-initial-") {
		t.Fatalf("first model call after compaction lacked the current target scope: %s", requests[1])
	}
	got, err := os.ReadFile(filepath.Join(root, "pkg/new.go"))
	if err != nil || string(got) != "applied after compaction" {
		t.Fatalf("write still needed another deferral after compaction: %q, %v", got, err)
	}
}

func TestGuidanceContinuationIsReadOnlyForVerification(t *testing.T) {
	cfg := guidanceFixture(t)
	cfg.Verification = &verification.Policy{Required: []verification.Check{{Name: "test", Command: "true"}}}
	putGuidanceFixture(t, cfg.Exec.Root, "AGENTS.md", strings.Repeat("instruction ", 800))
	// An unsupported workspace snapshot must not block a read-only turn.
	if err := os.Symlink("AGENTS.md", filepath.Join(cfg.Exec.Root, "snapshot-unsupported")); err != nil {
		t.Skipf("cannot create fixture symlink: %v", err)
	}
	s, err := cfg.Exec.Guidance.Resolve(context.Background(), "")
	if err != nil || len(s.Files) != 1 {
		t.Fatalf("snapshot files=%d err=%v", len(s.Files), err)
	}
	f := s.Files[0]
	read := verificationTool(tools.GuidanceTool, llm.GuidanceCursor{Target: s.Target, Path: f.Path, SHA256: f.SHA256, Offset: len(f.Prefix)})
	_, requests, err := runVerification(t, cfg, read)
	if err != nil || len(requests) != 2 || !strings.Contains(requests[1], "[project guidance") {
		t.Fatalf("a read-only continuation activated completion verification: requests=%d err=%v", len(requests), err)
	}
}

func TestGuidanceDoesNotReuseNestedVersionAcrossRootRefresh(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	putGuidanceFixture(t, root, "AGENTS.md", "original-root-rule")
	putGuidanceFixture(t, root, "pkg/AGENTS.md", "nested-rule")
	requestsDone := 0
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventStepUsage {
			requestsDone++
			if requestsDone == 2 {
				putGuidanceFixture(t, root, "AGENTS.md", "original-root-rule")
			}
		}
		if e.Kind == EventToolEnd && e.ToolCallID == "first" {
			putGuidanceFixture(t, root, "AGENTS.md", "changed-root-rule-must-invalidate-nested")
		}
	}
	write := func(id string) string {
		args, _ := json.Marshal(map[string]string{"path": "pkg/new.go", "content": id})
		return toolCallSSE(id, "write", string(args))
	}
	_, requests, err := runVerification(t, cfg, write("first"), write("stale"), write("new"))
	if err != nil || len(requests) != 4 || !strings.Contains(requests[1], "changed-root-rule-must-invalidate-nested") {
		t.Fatalf("root refresh fixture unsound: requests=%d err=%v", len(requests), err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "pkg/new.go")); err != nil || string(got) != "new" {
		t.Fatalf("write reused a superseded nested snapshot: got %q, err %v", got, err)
	}
}

func guidanceCursorFixture(t *testing.T, cfg *Config) llm.GuidanceCursor {
	t.Helper()
	snapshot, err := cfg.Exec.Guidance.Resolve(context.Background(), "")
	if err != nil || len(snapshot.Files) != 1 || !snapshot.Files[0].More {
		t.Fatalf("fixture does not advertise a continuation: %+v, %v", snapshot.Files, err)
	}
	f := snapshot.Files[0]
	return llm.GuidanceCursor{Target: snapshot.Target, Path: f.Path, SHA256: f.SHA256, Offset: len(f.Prefix)}
}

func guidanceReadCall(t *testing.T, id string, cursor llm.GuidanceCursor, limit int) llm.ToolCall {
	t.Helper()
	args, err := json.Marshal(struct {
		llm.GuidanceCursor
		Limit int `json:"limit"`
	}{cursor, limit})
	if err != nil {
		t.Fatal(err)
	}
	return budgetCall(id, tools.GuidanceTool, string(args))
}

func TestGuidanceContinuationRequiresOffsetFromTheModelRequest(t *testing.T) {
	cfg := guidanceFixture(t)
	putGuidanceFixture(t, cfg.Exec.Root, "AGENTS.md", strings.Repeat("p", guidance.PrefixBytes)+"more"+"last")
	first := guidanceCursorFixture(t, cfg)
	next := first
	next.Offset += 4
	guess := next
	guess.Offset++
	step := func(calls ...llm.ToolCall) string { return budgetSSE("", calls, nil, true) }
	events, _, err := runVerification(t, cfg,
		step(guidanceReadCall(t, "guess", guess, 4), guidanceReadCall(t, "first", first, 4), guidanceReadCall(t, "early-next", next, 4)),
		step(guidanceReadCall(t, "valid-next", next, 4)))
	if err != nil {
		t.Fatal(err)
	}
	results := make(map[string]Event)
	for _, e := range events {
		if e.Kind == EventToolEnd {
			results[e.ToolCallID] = e
		}
	}
	for _, id := range []string{"guess", "early-next"} {
		if e := results[id]; e.ToolErr == nil || !strings.Contains(e.ToolErr.Error(), "advertised") {
			t.Errorf("unpublished offset %s accepted: %+v", id, e)
		}
	}
	for id, suffix := range map[string]string{"first": "more", "valid-next": "last"} {
		if e := results[id]; e.ToolErr != nil || !strings.HasSuffix(e.ToolOutput, "\n"+suffix) {
			t.Errorf("published offset %s returned %+v, want %q", id, e, suffix)
		}
	}
}

func TestCompactionPreservesOnlyCurrentUnreadGuidance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed bool
		grace   bool
	}{
		{name: "unchanged"},
		{name: "changed", changed: true},
		{name: "step cap grace request", grace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := guidanceFixture(t)
			if tc.grace {
				cfg.MaxSteps = 1
			}
			const tail = "unread-guidance-tail-marker"
			guide := putGuidanceFixture(t, cfg.Exec.Root, "AGENTS.md", strings.Repeat("p", guidance.PrefixBytes)+tail)
			cursor := guidanceCursorFixture(t, cfg)
			cfg.ContextLimit = 12000
			compactions := 0
			cfg.CompactHistory = func(_ context.Context, history []llm.Message) (string, llm.Usage, error) {
				compactions++
				cfg.ContextLimit = 12000
				for _, m := range history {
					if strings.Contains(m.Content, tail) {
						t.Error("unread project guidance leaked into narration summary")
					}
				}
				if tc.changed {
					if err := os.WriteFile(guide, []byte("new-guidance-after-compaction"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return "The model requested a project source continuation.", llm.Usage{}, nil
			}
			cfg.OnEvent = func(e Event) {
				if e.Kind == EventToolEnd && e.ToolCallID == "read" && e.ToolErr == nil {
					cfg.ContextLimit = 3000
				}
			}
			read := guidanceReadCall(t, "read", cursor, guidance.PrefixBytes)
			_, requests, err := runVerification(t, cfg, budgetSSE("", []llm.ToolCall{read}, nil, true))
			settled := !tc.grace && err == nil || tc.grace && err != nil && cfg.LastTerminalReason == TerminalMaxTurns
			if !settled || compactions != 1 || len(requests) != 2 {
				t.Fatalf("requests=%d compactions=%d err=%v", len(requests), compactions, err)
			}
			if strings.Contains(requests[0], tail) || strings.Contains(requests[1], tail) == tc.changed {
				t.Fatalf("unread chunk delivery did not match its current digest (changed=%t)", tc.changed)
			}
			if tc.changed && !strings.Contains(requests[1], "new-guidance-after-compaction") {
				t.Fatal("compacted turn did not refresh the source")
			}
		})
	}
}

func TestCompactionFitsScopedGuidanceBeforeWritingCheckpoint(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	for _, name := range []string{"pkg/AGENTS.md", "pkg/CLAUDE.md", "pkg/deep/AGENTS.md", "pkg/deep/CLAUDE.md"} {
		putGuidanceFixture(t, root, name, strings.Repeat("p", guidance.PrefixBytes)+"-tail")
	}
	store, err := session.Open("bounded-guidance", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg.Session = store
	cfg.ContextLimit = 12000
	cfg.CompactHistory = func(_ context.Context, _ []llm.Message) (string, llm.Usage, error) {
		return "The nested write is waiting on project guidance.", llm.Usage{}, nil
	}
	commits := 0
	cfg.OnCompact = func(history []llm.Message, _ llm.Usage, archive string) {
		commits++
		if archive == "" || !cfg.checkpointContextFits(history, cfg.Tools) {
			t.Error("checkpoint omitted the complete next prompt budget or archive")
		}
		var paths int
		for _, m := range history {
			if m.Kind == guidanceMessageKind {
				paths += strings.Count(m.Content, "\nSource ")
			}
		}
		if paths != 4 {
			t.Errorf("checkpoint retained %d live source entries, want 4", paths)
		}
	}
	write := func(id string) string {
		return toolCallSSE(id, "write", `{"path":"pkg/deep/new.go","content":"after shortened preview"}`)
	}
	_, requests, err := runVerification(t, cfg, write("first"), write("retry"))
	if err != nil || len(requests) != 3 || commits != 1 {
		t.Fatalf("requests=%d checkpoints=%d err=%v", len(requests), commits, err)
	}
	if !strings.Contains(requests[1], "Source continues; use read_guidance") || !strings.Contains(requests[1], "pkg/deep/CLAUDE.md") {
		t.Fatal("checkpoint omitted the scoped source catalogue or continuation")
	}
	if data, err := os.ReadFile(filepath.Join(root, "pkg/deep/new.go")); err != nil || string(data) != "after shortened preview" {
		t.Fatalf("write after compacted guidance = %q, %v", data, err)
	}
}

func TestGuidanceAnnouncesNestedRemovalAfterRootRefresh(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	putGuidanceFixture(t, root, "AGENTS.md", "old-root-rule")
	nested := putGuidanceFixture(t, root, "pkg/AGENTS.md", "nested-rule-that-must-be-cleared")
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "first" {
			putGuidanceFixture(t, root, "AGENTS.md", "new-root-rule")
			if err := os.Remove(nested); err != nil {
				t.Fatal(err)
			}
		}
	}
	write := func(id string) string {
		args, _ := json.Marshal(map[string]string{"path": "pkg/new.go", "content": id})
		return toolCallSSE(id, "write", string(args))
	}
	_, requests, err := runVerification(t, cfg, write("first"), write("stale"), write("updated"))
	if err != nil || len(requests) != 4 {
		t.Fatalf("requests=%d err=%v", len(requests), err)
	}
	index := strings.LastIndex(requests[2], "[harness project guidance")
	if index < 0 || !strings.Contains(requests[2][index:], "new-root-rule") || strings.Contains(requests[2][index:], "nested-rule-that-must-be-cleared") {
		t.Fatal("previous nested guidance was not superseded for the mutation directory")
	}
	if got, err := os.ReadFile(filepath.Join(root, "pkg/new.go")); err != nil || string(got) != "updated" {
		t.Fatalf("write reused withdrawn nested instructions: got %q, err %v", got, err)
	}
}

func TestGuidanceGraceCheckpointUsesToollessRequestBudget(t *testing.T) {
	cfg := guidanceFixture(t)
	cfg.MaxSteps = 1
	cfg.ContextLimit = 60000
	cfg.Tools = append(cfg.Tools, llm.Tool("large_schema", strings.Repeat("d", 12000), `{"type":"object"}`))
	const marker = "last-bytes-of-unread-chunk"
	putGuidanceFixture(t, cfg.Exec.Root, "AGENTS.md", strings.Repeat("p", guidance.PrefixBytes)+strings.Repeat("b", guidance.PrefixBytes-len(marker))+marker)
	cursor := guidanceCursorFixture(t, cfg)
	cfg.CompactHistory = func(_ context.Context, _ []llm.Message) (string, llm.Usage, error) {
		return "The model requested a project source continuation.", llm.Usage{}, nil
	}
	cfg.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd && e.ToolCallID == "read" && e.ToolErr == nil {
			cfg.ContextLimit = 5800
		}
	}
	read := guidanceReadCall(t, "read", cursor, guidance.PrefixBytes)
	_, requests, err := runVerification(t, cfg, budgetSSE("", []llm.ToolCall{read}, nil, true))
	if err == nil || cfg.LastTerminalReason != TerminalMaxTurns || len(requests) != 2 {
		t.Fatalf("tool-less grace did not run after compaction: requests=%d reason=%q err=%v", len(requests), cfg.LastTerminalReason, err)
	}
	if !strings.Contains(requests[1], marker) || strings.Contains(requests[1], `"tools"`) {
		t.Fatal("grace did not include the complete unread chunk in its tool-less request")
	}
}

func TestCompactionCanPruneAfterRetiringEarlierGuidance(t *testing.T) {
	cfg := guidanceFixture(t)
	root := cfg.Exec.Root
	putGuidanceFixture(t, root, "AGENTS.md", "current-root-instructions")
	store, err := session.Open("prune-guidance", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg.Session = store
	cfg.ContextLimit = 40000
	cfg.CompactHistory = func(_ context.Context, _ []llm.Message) (string, llm.Usage, error) {
		t.Error("transcript differing only by archived guidance should not request a summary")
		return "unnecessary summary", llm.Usage{}, nil
	}
	history := []llm.Message{
		{Role: "user", Kind: "task", Content: "old task"},
		{Role: "user", Kind: guidanceMessageKind, Content: "retired-guidance-marker"},
	}
	for i := 0; i < 8; i++ {
		history = appendFixtureBatch(history, i, 16000)
	}
	history = append(history, llm.Message{Role: "user", Kind: "task", Content: "new task"})
	for _, m := range history {
		if err := store.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	g := newGuidanceTurn(cfg.Exec)
	visible := g.refreshRoot(ctx, cfg, WithoutHistoricalGuidance(history))
	if !cfg.needsCompact(visible) {
		t.Fatal("fixture does not reach the compaction budget")
	}
	out, _, err := cfg.compactInLoop(ctx, visible, cfg.prepareNextPrompt(ctx, cfg.Tools, g))
	if err != nil {
		t.Fatal(err)
	}
	pruned := false
	for _, m := range out {
		pruned = pruned || m.Content == prunedToolResultPlaceholder
		if strings.Contains(m.Content, "retired-guidance-marker") {
			t.Fatal("pruning reintroduced retired guidance into the prompt")
		}
	}
	if !pruned {
		t.Fatal("expected raw checkpoint to prune earlier tool results")
	}
}
