package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

// fakePluginRunner stands in for the plugin registry in loop tests:
// it denies, rewrites args, or rewrites results as configured.
type fakePluginRunner struct {
	deny      string            // deny every tool_call with this reason ("" = allow)
	rewrite   map[string]string // tool -> replacement args JSON
	result    map[string]string // tool -> replacement observation
	callCalls int
}

func (h *fakePluginRunner) Has(_ string) bool { return false }

func (h *fakePluginRunner) Call(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

func (h *fakePluginRunner) RunToolCallHooks(_ context.Context, tool, argsJSON string) (string, string) {
	h.callCalls++
	if h.deny != "" {
		return argsJSON, h.deny
	}
	if r, ok := h.rewrite[tool]; ok {
		return r, ""
	}
	return argsJSON, ""
}

func (h *fakePluginRunner) RunToolResultHooks(_ context.Context, tool, _, result string, _ error) string {
	if r, ok := h.result[tool]; ok {
		return r
	}
	return result
}

// toolCallSSE renders one tool-call turn as chat SSE, with the
// arguments split across two fragments (as real streams do).
func toolCallSSE(id, name, args string) string {
	half := len(args) / 2
	esc := func(s string) string {
		bs, _ := json.Marshal(s)
		return string(bs[1 : len(bs)-1])
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n", id, name, esc(args[:half]))
	fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%q}}]}}]}\n\n", esc(args[half:]))
	sb.WriteString("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func textSSE(text string) string {
	content, _ := json.Marshal(text)
	return "data: {\"choices\":[{\"delta\":{\"content\":" + string(content) + "}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n" +
		"data: [DONE]\n\n"
}

// runTwoTurns runs a mock two-turn conversation (a tool call, then a
// final text turn) and returns the events and the request body of the
// second turn, which carries the tool observation the model saw.
func runTwoTurns(t *testing.T, cfg *Config, firstTurn string) ([]Event, string) {
	t.Helper()
	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			fmt.Fprint(w, firstTurn)
			return
		}
		fmt.Fprint(w, textSSE("done"))
	}))
	defer srv.Close()
	cfg.Client.BaseURL = srv.URL
	var events []Event
	cfg.OnEvent = func(e Event) { events = append(events, e) }
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want done", text)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2", len(bodies))
	}
	return events, bodies[1]
}

func TestHookDenySkipsApprovalAndReachesModel(t *testing.T) {
	root := t.TempDir()
	hooks := &fakePluginRunner{deny: "gate: no writes allowed"}
	asked := false
	cfg := &Config{
		Client: &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:   &tools.Executor{Root: root, Plugins: hooks},
		Tools:  ToolDefs(),
		System: "test system",
		Mode:   "suggest",
		Ask: func(string) (bool, error) {
			asked = true
			return true, nil
		},
		MaxSteps: 5,
	}
	events, secondBody := runTwoTurns(t, cfg, toolCallSSE("call_1", "write", `{"path":"x.txt","content":"hi"}`))
	if asked {
		t.Fatal("approval was asked for a hook-denied call")
	}
	if hooks.callCalls != 1 {
		t.Fatalf("tool_call hooks ran %d times, want 1", hooks.callCalls)
	}
	for _, e := range events {
		if e.Kind == EventApproval {
			t.Fatal("approval event emitted for a hook-denied call")
		}
	}
	var denied *Event
	for i := range events {
		if events[i].Kind == EventToolEnd && events[i].ToolName == "write" {
			denied = &events[i]
		}
	}
	if denied == nil {
		t.Fatal("no tool_end event for the denied write")
	}
	if denied.ToolErr == nil {
		t.Fatal("denied tool_end has no ToolErr")
	}
	if !strings.Contains(denied.ToolOutput, "gate: no writes allowed") {
		t.Fatalf("denial observation = %q, want the reason in it", denied.ToolOutput)
	}
	if !strings.Contains(secondBody, "Denied by plugin gate: no writes allowed") {
		t.Fatal("the model never saw the denial observation")
	}
}

func TestHookRewriteReachesDispatch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("aaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("bbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := &fakePluginRunner{rewrite: map[string]string{"read": `{"path":"b.txt"}`}}
	cfg := &Config{
		Client:   &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root, Plugins: hooks},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
	}
	_, secondBody := runTwoTurns(t, cfg, toolCallSSE("call_1", "read", `{"path":"a.txt"}`))
	if !strings.Contains(secondBody, "bbb") {
		t.Fatal("the model never saw the rewritten file's content")
	}
	if strings.Contains(secondBody, "aaa") {
		t.Fatal("the model saw the original file's content despite the rewrite")
	}
}

func TestHookRewriteReachesApprovalPrompt(t *testing.T) {
	root := t.TempDir()
	hooks := &fakePluginRunner{rewrite: map[string]string{"write": `{"path":"x.txt","content":"rewritten"}`}}
	var prompt string
	cfg := &Config{
		Client: &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:   &tools.Executor{Root: root, Plugins: hooks},
		Tools:  ToolDefs(),
		System: "test system",
		Mode:   "suggest",
		Ask: func(p string) (bool, error) {
			prompt = p
			return true, nil
		},
		MaxSteps: 5,
	}
	runTwoTurns(t, cfg, toolCallSSE("call_1", "write", `{"path":"x.txt","content":"original"}`))
	if !strings.Contains(prompt, "rewritten") {
		t.Fatalf("approval prompt = %q, want the rewritten args in it", prompt)
	}
	raw, err := os.ReadFile(filepath.Join(root, "x.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "rewritten" {
		t.Fatalf("x.txt = %q, want the rewritten content", raw)
	}
}

func TestToolResultHookRewritesObservation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := &fakePluginRunner{result: map[string]string{"read": "REPLACED"}}
	cfg := &Config{
		Client:   &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root, Plugins: hooks},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
	}
	_, secondBody := runTwoTurns(t, cfg, toolCallSSE("call_1", "read", `{"path":"a.txt"}`))
	if !strings.Contains(secondBody, "REPLACED") {
		t.Fatal("the model never saw the hook-rewritten observation")
	}
	if strings.Contains(secondBody, "hello") {
		t.Fatal("the model saw the original result despite the rewrite")
	}
}

func TestToolResultHookSkipsBash(t *testing.T) {
	// Hooks run on the Dispatch path only, so a bash observation is
	// never rewritten: this pins the documented limitation.
	root := t.TempDir()
	hooks := &fakePluginRunner{result: map[string]string{"bash": "REPLACED"}}
	cfg := &Config{
		Client:   &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root, Plugins: hooks},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
	}
	_, secondBody := runTwoTurns(t, cfg, toolCallSSE("call_1", "bash", `{"command":"echo hi"}`))
	if strings.Contains(secondBody, "REPLACED") {
		t.Fatal("bash observation was rewritten, but hooks only run on Dispatch")
	}
	if !strings.Contains(secondBody, "hi") {
		t.Fatal("the model never saw the real bash output")
	}
}

// A tool call for a tool the turn did not offer (write in plan
// mode) is refused before approval and never dispatched: the
// offered list is the enforcement, not a hint. Without this, a
// model that hallucinates write from the system prompt — which
// still lists all seven built-ins — would bypass plan mode.
func TestNonOfferedToolRefused(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{
		Client:   &llm.Client{APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolsForMode(ToolModePlan, ToolDefs()),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
	}
	events, secondBody := runTwoTurns(t, cfg, toolCallSSE("call_1", "write", `{"path":"x.txt","content":"hi"}`))
	if _, err := os.Stat(filepath.Join(root, "x.txt")); err == nil {
		t.Fatal("a non-offered write call created the file")
	}
	if !strings.Contains(secondBody, "Tool write is not available in this turn") {
		t.Fatal("the model never saw the not-available observation")
	}
	var refused *Event
	for i := range events {
		if events[i].Kind == EventToolEnd && events[i].ToolName == "write" {
			refused = &events[i]
		}
	}
	if refused == nil || refused.ToolErr == nil {
		t.Fatal("no tool_end with ToolErr for the refused write")
	}
}
