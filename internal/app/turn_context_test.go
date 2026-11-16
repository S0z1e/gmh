package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/agent"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

func TestTurnContextKeepsWirePrefixAcrossTurnsAndResume(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OPENAI_API_KEY", "fake")
			root := t.TempDir()
			statePath := filepath.Join(root, ".agents", "plugins", "state", "data", "state.txt")
			if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, []byte("Open todos: old"), 0600); err != nil {
				t.Fatal(err)
			}
			writeMockProvider(t, root)
			writePhase4Plugin(t, root, "state", `harness.on("before_agent_start", () => harness.readFile("state.txt"));`)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch api {
				case "chat":
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				case "responses":
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
				case "anthropic":
					fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			defer srv.Close()
			t.Setenv("OPENAI_BASE_URL", srv.URL)
			a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, API: api, Mode: "auto", MaxSteps: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = a.Store.Close() }()
			var traces []llm.RequestTrace
			a.Cfg.OnEvent = func(e agent.Event) {
				if e.Request != nil {
					traces = append(traces, *e.Request)
				}
			}
			r := &Runner{App: a, Print: func(string, ...any) {}}
			r.Handle(context.Background(), "first task")
			if r.LastErr != nil {
				t.Fatal(r.LastErr)
			}
			if err := os.WriteFile(statePath, []byte(""), 0600); err != nil {
				t.Fatal(err)
			}
			r.Handle(context.Background(), "second task")
			if r.LastErr != nil {
				t.Fatal(r.LastErr)
			}
			if len(traces) != 2 || traces[1].Relation != "append" || traces[1].SystemChanged || traces[1].ToolsChanged {
				t.Fatalf("second-turn trace=%+v", traces)
			}
			if len(a.History) != 6 || a.History[1].Kind != "turn_context" || a.History[4].Kind != "turn_context" {
				t.Fatalf("unexpected history: %+v", a.History)
			}
			if !strings.Contains(a.History[1].Content, "Open todos: old") || strings.Contains(a.History[4].Content, "Open todos:") {
				t.Fatal("new empty snapshot did not supersede old todos")
			}
			loaded, err := session.Load(a.Store.ID)
			if err != nil || loaded[1].Content != a.History[1].Content || loaded[4].Content != a.History[4].Content {
				t.Fatalf("snapshot did not survive reload: %v", err)
			}
			if _, err := a.LoadSession(a.Store.ID); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, []byte("Open todos: new"), 0600); err != nil {
				t.Fatal(err)
			}
			r.Handle(context.Background(), "after resume")
			if r.LastErr != nil || len(traces) != 3 || traces[2].Relation != "append" {
				t.Fatalf("resume trace=%+v err=%v", traces, r.LastErr)
			}
			if !strings.Contains(a.History[len(a.History)-2].Content, "Open todos: new") {
				t.Fatal("resumed turn missed fresh plugin state")
			}
			if r.Turns != 3 {
				t.Fatal("context snapshots counted as turns")
			}
		})
	}
}

func TestTurnContextCapturesModeChangedByHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "fake")
	root := t.TempDir()
	writeMockProvider(t, root)
	writePhase4Plugin(t, root, "mode", `harness.on("before_agent_start", () => { harness.setMode("plan"); return "plugin state"; });`)
	var bodies [][]byte
	srv := recordingServer(t, &bodies)
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto", MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Store.Close()
	r := &Runner{App: a, Print: func(string, ...any) {}}
	r.Handle(context.Background(), "task")
	if r.LastErr != nil || len(bodies) != 1 {
		t.Fatalf("turn=%v, calls=%d", r.LastErr, len(bodies))
	}
	var req struct {
		Messages []llm.Message `json:"messages"`
		Tools    []llm.ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(bodies[0], &req); err != nil {
		t.Fatal(err)
	}
	snapshot := req.Messages[len(req.Messages)-1].Content
	if a.ToolMode() != agent.ToolModePlan || !strings.Contains(snapshot, "Tool mode: plan") || !strings.Contains(snapshot, agent.PlanModeReminder) || a.pendingReminder != "" {
		t.Fatal("snapshot did not capture hook-selected mode and reminder")
	}
	for _, tool := range req.Tools {
		if tool.Function.Name == "write" {
			t.Fatal("plan snapshot sent with default tools")
		}
	}
}
