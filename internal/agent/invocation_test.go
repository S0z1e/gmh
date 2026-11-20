package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestEffectiveInvocationIsDurableBeforeApproval(t *testing.T) {
	for _, tc := range []struct {
		name               string
		hook, continuation bool
	}{
		{"hook", true, false}, {"continuation", false, true}, {"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			store, err := session.New("effective")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			root := t.TempDir()
			original := `{"path":"requested","content":"data"}`
			hooked := `{"path":"hooked","content":"data"}`
			final := original
			hooks := &fakePluginRunner{}
			if tc.hook {
				hooks.rewrite = map[string]string{"write": hooked}
				final = hooked
			}
			cfg := &Config{Client: &llm.Client{APIKey: "test", Model: "test", API: "chat"}, Exec: &tools.Executor{Root: root, Plugins: hooks}, Tools: ToolDefs(), Session: store, Mode: "suggest", MaxSteps: 3}
			if tc.continuation {
				cfg.DoomLoopThreshold = 1
				final = `{"path":"actual","content":"data"}`
				cfg.ConfirmContinuation = func(ContinuationRequest) (ContinuationDecision, error) {
					return ContinuationDecision{Outcome: ContinuationOnce, UpdatedArgs: final}, nil
				}
			}
			var approved ApprovalRequest
			cfg.Ask = func(req ApprovalRequest) (bool, error) {
				approved = req
				loaded, err := session.Load(store.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(loaded[0].ToolInvocations) != 1 || loaded[0].ToolCalls[0].Function.Arguments != final || loaded[0].ToolInvocations[0].ID != req.InvocationID || req.Arguments != final {
					t.Fatalf("approval not backed by durable effective invocation: %+v, %+v", req, loaded)
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatalf("execution happened before approval: %v %v", entries, err)
				}
				return true, nil
			}
			events, wire := runTwoTurns(t, cfg, toolCallSSE("call_1", "write", original))
			if approved.InvocationID == "" || approved.ToolCallID != "call_1" {
				t.Fatal("approval lacks invocation identity")
			}
			loaded, err := session.Load(store.ID)
			if err != nil {
				t.Fatal(err)
			}
			inv := loaded[0].ToolInvocations[0]
			if inv.OriginalArguments != original || inv.Arguments != final || loaded[1].InvocationID != inv.ID {
				t.Fatalf("lost execution provenance: %+v", loaded)
			}
			wantSources := []string{}
			if tc.hook {
				wantSources = append(wantSources, "plugin_hook")
			}
			if tc.continuation {
				wantSources = append(wantSources, "continuation")
			}
			if len(inv.Rewrites) != len(wantSources) {
				t.Fatalf("rewrites = %+v", inv.Rewrites)
			}
			for i, source := range wantSources {
				if inv.Rewrites[i].Source != source {
					t.Fatalf("rewrite order: %+v", inv.Rewrites)
				}
			}
			starts := 0
			for _, event := range events {
				if event.Kind == EventToolStart {
					starts++
					if event.ToolArgs != final || event.InvocationID != inv.ID || event.ToolCallID != "call_1" {
						t.Fatalf("incorrect tool_start: %+v", event)
					}
				}
				if event.Kind == EventToolEnd && event.InvocationID != inv.ID {
					t.Fatalf("result identity: %+v", event)
				}
			}
			if starts != 1 {
				t.Fatalf("tool starts = %d", starts)
			}
			if strings.Contains(wire, "tool_invocations") || strings.Contains(wire, "invocation_id") {
				t.Fatalf("harness metadata leaked to provider: %s", wire)
			}
			data, err := os.ReadFile(store.Path)
			if err != nil {
				t.Fatal(err)
			}
			var raw llm.Message
			if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 3)[1]), &raw); err != nil {
				t.Fatal(err)
			}
			if raw.ToolCalls[0].Function.Arguments != original {
				t.Fatalf("original request overwritten: %+v", raw)
			}
			if err := store.Rewrite(loaded); err != nil {
				t.Fatal(err)
			}
			again, err := session.Load(store.ID)
			if err != nil || again[0].ToolInvocations[0].OriginalArguments != original || again[0].ToolCalls[0].Function.Arguments != final {
				t.Fatalf("rewrite lost provenance: %+v %v", again, err)
			}
			var args tools.WriteArgs
			if err := json.Unmarshal([]byte(final), &args); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(filepath.Join(root, args.Path))
			if err != nil || string(data) != "data" {
				t.Fatalf("effective write did not run: %q %v", data, err)
			}
		})
	}
}

func TestInvocationSaveFailurePreventsApprovalAndExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("fail-invocation")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, toolCallSSE("call_1", "write", `{"path":"file","content":"data"}`))
	}))
	defer srv.Close()
	asks := 0
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "test", API: "chat"}, Exec: &tools.Executor{Root: root}, Tools: ToolDefs(), Session: store, Mode: "suggest", DoomLoopThreshold: 1,
		Ask: func(ApprovalRequest) (bool, error) { asks++; return true, nil },
		ConfirmContinuation: func(ContinuationRequest) (ContinuationDecision, error) {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			return ContinuationDecision{Outcome: ContinuationOnce}, nil
		},
	}
	_, _, err = cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, os.ErrClosed) || asks != 0 {
		t.Fatalf("save failure did not stop approval: %v asks=%d", err, asks)
	}
	if _, err := os.Stat(filepath.Join(root, "file")); !os.IsNotExist(err) {
		t.Fatalf("write ran without durable invocation: %v", err)
	}
}

func TestCanceledBatchReplaysOnlyCommittedEffectiveInvocations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("canceled-batch")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	calls := []map[string]any{
		{"index": 0, "id": "first", "function": map[string]string{"name": "write", "arguments": `{"path":"requested","content":"data"}`}},
		{"index": 1, "id": "second", "function": map[string]string{"name": "write", "arguments": `{"path":"never","content":"data"}`}},
	}
	chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": calls}}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks := &fakePluginRunner{rewrite: map[string]string{"write": `{"path":"actual","content":"data"}`}}
	cfg := &Config{
		Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "test", API: "chat"}, Exec: &tools.Executor{Root: root, Plugins: hooks}, Tools: ToolDefs(), Session: store, Mode: "auto",
		OnEvent: func(e Event) {
			if e.Kind == EventToolEnd {
				cancel()
			}
		},
	}
	_, _, err = cfg.Run(ctx, []llm.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error: %v", err)
	}
	loaded, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 || len(loaded[0].ToolInvocations) != 1 || loaded[0].ToolCalls[0].Function.Arguments != `{"path":"actual","content":"data"}` || loaded[0].ToolCalls[1].Function.Arguments != `{"path":"never","content":"data"}` {
		t.Fatalf("recovered wrong arguments: %+v", loaded)
	}
	if loaded[1].InvocationID == "" || loaded[2].InvocationID != "" || loaded[2].Kind != "interrupted" {
		t.Fatalf("recovered wrong result identities: %+v", loaded)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "actual" || hooks.callCalls != 1 {
		t.Fatalf("wrong side effects: %v %v hooks=%d", entries, err, hooks.callCalls)
	}
}

func TestContinuationDenialCommitsEffectiveInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("denied-invocation")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	original := `{"path":"requested","content":"data"}`
	actual := `{"path":"actual","content":"data"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, toolCallSSE("call", "write", original))
	}))
	defer srv.Close()
	var events []Event
	cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "test", API: "chat"}, Exec: &tools.Executor{Root: root, Plugins: &fakePluginRunner{rewrite: map[string]string{"write": actual}}}, Tools: ToolDefs(), Session: store, Mode: "auto", DoomLoopThreshold: 1,
		ConfirmContinuation: func(ContinuationRequest) (ContinuationDecision, error) {
			return ContinuationDecision{Outcome: ContinuationDeny}, nil
		},
		OnEvent: func(e Event) { events = append(events, e) },
	}
	_, _, err = cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, ErrRepeatedToolCallDenied) {
		t.Fatalf("Run error: %v", err)
	}
	loaded, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || len(loaded[0].ToolInvocations) != 1 || loaded[0].ToolCalls[0].Function.Arguments != actual {
		t.Fatalf("denial lost effective invocation: %+v", loaded)
	}
	id := loaded[0].ToolInvocations[0].ID
	if id == "" || loaded[1].InvocationID != id {
		t.Fatalf("denial has dangling identity: %+v", loaded)
	}
	for _, e := range events {
		if e.Kind == EventToolStart || e.Kind == EventToolEnd {
			if e.InvocationID != id {
				t.Fatalf("event identity: %+v", e)
			}
		}
	}
	data, err := os.ReadFile(store.AuditPath())
	if err != nil || !strings.Contains(string(data), id) {
		t.Fatalf("audit identity: %q %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("denied invocation executed: %v %v", entries, err)
	}
}

func TestEarlyRejectionsDoNotPublishUncommittedInvocationIDs(t *testing.T) {
	for _, reason := range []string{"unoffered", "invalid_original", "hook_denied", "invalid_hook", "invalid_continuation"} {
		t.Run(reason, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			store, err := session.New("early-rejection")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			hooks := &fakePluginRunner{}
			args := `{"path":"file","content":"data"}`
			cfg := &Config{Client: &llm.Client{APIKey: "test", Model: "test", API: "chat"}, Exec: &tools.Executor{Root: t.TempDir(), Plugins: hooks}, Tools: ToolDefs(), Session: store, Mode: "auto", MaxSteps: 3}
			switch reason {
			case "unoffered":
				cfg.Tools = ToolsForMode(ToolModePlan, ToolDefs())
			case "invalid_original":
				args = `{"path":"file"}`
			case "hook_denied":
				hooks.deny = "policy"
			case "invalid_hook":
				hooks.rewrite = map[string]string{"write": `{"path":"file"}`}
			case "invalid_continuation":
				cfg.DoomLoopThreshold = 1
				cfg.ConfirmContinuation = func(ContinuationRequest) (ContinuationDecision, error) {
					return ContinuationDecision{Outcome: ContinuationOnce, UpdatedArgs: `{"path":"file"}`}, nil
				}
			}
			events, _ := runTwoTurns(t, cfg, toolCallSSE("call", "write", args))
			for _, e := range events {
				if e.InvocationID != "" {
					t.Fatalf("uncommitted ID in event: %+v", e)
				}
			}
			loaded, err := session.Load(store.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded[0].ToolInvocations) != 0 || loaded[1].InvocationID != "" {
				t.Fatalf("uncommitted ID in replay: %+v", loaded)
			}
		})
	}
}
