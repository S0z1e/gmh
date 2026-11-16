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
	"sync/atomic"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestCorruptReplyCannotCommitOrExecuteTools(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%v", api, retry), func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				store, err := session.New("protocol")
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				event := func(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }
				args := `{"path":"effect","content":"once"}`
				var body, terminal, answer string
				switch api {
				case "chat":
					terminal = "data: [DONE]\n\n"
					body = event(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "partial", "tool_calls": []any{map[string]any{"index": 0, "id": "call", "type": "function", "function": map[string]any{"name": "write", "arguments": args}}}}}}})
					answer = event(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "final"}}}}) + terminal
				case "responses":
					terminal = event(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{}}})
					body = event(map[string]any{"type": "response.output_text.delta", "delta": "partial"}) + event(map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": "item", "type": "function_call", "call_id": "call", "name": "write", "arguments": ""}}) + event(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "item", "delta": args})
					answer = event(map[string]any{"type": "response.output_text.delta", "delta": "final"}) + terminal
				case "anthropic":
					terminal = event(map[string]any{"type": "message_stop"})
					body = event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": "partial"}}) + event(map[string]any{"type": "content_block_stop", "index": 0}) + event(map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "call", "name": "write", "input": map[string]any{}}}) + event(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}}) + event(map[string]any{"type": "content_block_stop", "index": 1})
					answer = event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": "final"}}) + event(map[string]any{"type": "content_block_stop", "index": 0}) + terminal
				}
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch calls.Add(1) {
					case 1:
						fmt.Fprint(w, body+"data: {BROKEN\n\n"+terminal)
					case 2:
						fmt.Fprint(w, body+terminal)
					default:
						fmt.Fprint(w, answer)
					}
				}))
				defer srv.Close()
				root := t.TempDir()
				var events []Event
				approvals := 0
				cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "test", Model: "test", API: api}, Exec: &tools.Executor{Root: root}, Tools: ToolDefs(), Mode: "suggest", Session: store, MaxSteps: 3, MaxLLMAttempts: 1, OnEvent: func(e Event) { events = append(events, e) }, Ask: func(ApprovalRequest) (bool, error) { approvals++; return true, nil }}
				if retry {
					cfg.MaxLLMAttempts = 2
				}
				text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "write"}})
				loaded, loadErr := session.Load(store.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if countKind(events, EventStepDiscard) != 1 {
					t.Fatalf("missing discard: %+v", events)
				}
				if !retry {
					var pe *llm.ProtocolError
					if !errors.As(err, &pe) || approvals != 0 || countKind(events, EventToolStart) != 0 || len(loaded) != 0 {
						t.Fatalf("invalid reply committed: approvals=%d history=%+v err=%v", approvals, loaded, err)
					}
					if _, err := os.Stat(filepath.Join(root, "effect")); !os.IsNotExist(err) {
						t.Fatalf("side effect: %v", err)
					}
				} else {
					if err != nil || text != "final" || calls.Load() != 3 || approvals != 1 || countKind(events, EventToolStart) != 1 {
						t.Fatalf("retry: text=%q calls=%d approvals=%d err=%v", text, calls.Load(), approvals, err)
					}
					data, err := os.ReadFile(store.Path)
					if err != nil || strings.Contains(string(data), "BROKEN") {
						t.Fatalf("corrupt transcript %s %v", data, err)
					}
				}
			})
		}
	}
}
