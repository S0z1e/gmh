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
	path := putGuidanceFixture(t, cfg.Exec.Root, "AGENTS.md", "instructions")
	// An unsupported workspace snapshot must not block a read-only turn.
	if err := os.Symlink("AGENTS.md", filepath.Join(cfg.Exec.Root, "snapshot-unsupported")); err != nil {
		t.Skipf("cannot create fixture symlink: %v", err)
	}
	s, err := cfg.Exec.Guidance.Resolve(context.Background(), "")
	if err != nil || len(s.Files) != 1 {
		t.Fatalf("snapshot files=%d err=%v", len(s.Files), err)
	}
	read := verificationTool(tools.GuidanceTool, map[string]any{"target": s.Target, "path": path, "sha256": s.Files[0].SHA256, "offset": 0})
	_, requests, err := runVerification(t, cfg, read)
	if err != nil || len(requests) != 2 || !strings.Contains(requests[1], "[project guidance") {
		t.Fatalf("a read-only continuation activated completion verification: requests=%d err=%v", len(requests), err)
	}
}
