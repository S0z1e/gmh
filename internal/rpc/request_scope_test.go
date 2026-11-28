package rpc

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/app"
)

// TestReplayedRequestIDsFromEarlierProcessAreRejected verifies that
// approval_request, continuation_request, and question_request IDs are scoped
// to their server process's connection_id, and that responses using IDs from
// an earlier process's replayed log (or naming an earlier connection_id) do
// not resolve pending requests on the second process.
func TestReplayedRequestIDsFromEarlierProcessAreRejected(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		step1        fakeStep
		steps2       []fakeStep
		prompt1      string
		prompt2      string
		eventType    string
		responseType string
		idField      string
		extraFields  map[string]any
		verifyAfter  func(t *testing.T, root string, settled map[string]any)
	}{
		{
			name:         "approval",
			mode:         "suggest",
			step1:        fakeStep{kind: "tool", tool: "write", args: map[string]any{"path": "p1.txt", "content": "one"}},
			steps2:       []fakeStep{{kind: "tool", tool: "write", args: map[string]any{"path": "p2.txt", "content": "two"}}, {kind: "text", text: "approved p2"}},
			prompt1:      "Write p1.txt",
			prompt2:      "Write p2.txt",
			eventType:    "approval_request",
			responseType: "approval_response",
			idField:      "approval_id",
			extraFields:  map[string]any{"approved": true},
			verifyAfter: func(t *testing.T, root string, settled map[string]any) {
				t.Helper()
				if settled["error"] != nil {
					t.Fatalf("settled with error: %v", settled["error"])
				}
				if _, err := os.Stat(filepath.Join(root, "p1.txt")); !os.IsNotExist(err) {
					t.Fatalf("p1.txt should not exist, err=%v", err)
				}
				if got, err := os.ReadFile(filepath.Join(root, "p2.txt")); err != nil || string(got) != "two" {
					t.Fatalf("p2.txt = %q, err=%v", got, err)
				}
			},
		},
		{
			name:         "continuation",
			mode:         "auto",
			step1:        fakeStep{kind: "tool", tool: "read", args: map[string]any{"path": "t.txt"}},
			steps2:       []fakeStep{{kind: "tool", tool: "read", args: map[string]any{"path": "t.txt"}}},
			prompt1:      "Read t.txt",
			prompt2:      "Read t.txt again",
			eventType:    "continuation_request",
			responseType: "continuation_response",
			idField:      "continuation_id",
			extraFields:  map[string]any{"decision": "deny"},
			verifyAfter: func(t *testing.T, _ string, settled map[string]any) {
				t.Helper()
				if settled["reason"] != "repeated_tool_call_denied" {
					t.Fatalf("settled reason = %v, want repeated_tool_call_denied", settled["reason"])
				}
			},
		},
		{
			name:         "question",
			mode:         "auto",
			step1:        fakeStep{kind: "tool", tool: "ask_user", args: map[string]any{"questions": []any{map[string]any{"question": "Which DB?", "options": []any{map[string]any{"label": "SQLite"}, map[string]any{"label": "Postgres"}}}}}},
			steps2:       []fakeStep{{kind: "tool", tool: "ask_user", args: map[string]any{"questions": []any{map[string]any{"question": "Which DB?", "options": []any{map[string]any{"label": "SQLite"}, map[string]any{"label": "Postgres"}}}}}}, {kind: "text", text: "using Postgres"}},
			prompt1:      "Pick a DB",
			prompt2:      "Pick a DB again",
			eventType:    "question_request",
			responseType: "question_response",
			idField:      "question_id",
			extraFields:  map[string]any{"answers": []any{map[string]any{"question": 0, "selected": []string{"Postgres"}}}},
			verifyAfter: func(t *testing.T, _ string, settled map[string]any) {
				t.Helper()
				if settled["error"] != nil {
					t.Fatalf("settled with error: %v", settled["error"])
				}
				if text, _ := settled["text"].(string); !strings.Contains(text, "Postgres") {
					t.Fatalf("settled text = %q, want Postgres", text)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a1, runner1, root := setupTestApp(t, tc.mode, []fakeStep{tc.step1})
			sessionID := a1.Store.ID

			d1 := startDriver(t, a1, runner1)
			ready1 := d1.waitFor("ready", 5*time.Second)
			conn1, _ := ready1["connection_id"].(string)
			if conn1 == "" {
				t.Fatal("process 1 ready missing connection_id")
			}
			d1.send(map[string]any{"id": "p1", "type": "prompt", "message": tc.prompt1})
			d1.waitForResponse("p1", 5*time.Second)
			ev1 := d1.waitFor(tc.eventType, 10*time.Second)
			id1, _ := ev1["id"].(string)
			if id1 == "" || !strings.Contains(id1, conn1) {
				t.Fatalf("process 1 %s id = %q, want it scoped to connection_id %q", tc.eventType, id1, conn1)
			}
			d1.send(map[string]any{"id": "ab1", "type": "abort"})
			d1.waitForResponse("ab1", 5*time.Second)
			d1.waitFor("agent_settled", 10*time.Second)
			d1.quit(t)
			_ = a1.Store.Close()

			// Process 2 resumes the exact same session with a fresh connection_id.
			srv2 := httptest.NewServer(fakeHandler(t, tc.steps2))
			t.Cleanup(srv2.Close)
			cfg := `{"providers": {"mock": {"apiKey": "$OPENAI_API_KEY", "baseUrl": "` + srv2.URL + `/v1", "api": "chat", "models": [{"id": "mock", "input": ["text", "image"]}]}}}`
			if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			a2, err := app.Setup(app.Params{
				WorkspaceTrust:             "trust",
				Root:                       root,
				Resume:                     sessionID,
				Mode:                       tc.mode,
				MaxSteps:                   10,
				BackgroundThresholdSeconds: 0,
				DisableBackground:          true,
			})
			if err != nil {
				t.Fatalf("setup process 2: %v", err)
			}
			t.Cleanup(func() { _ = a2.Store.Close() })
			runner2 := &app.Runner{App: a2, Print: func(string, ...any) {}}

			d2 := startDriver(t, a2, runner2)
			ready2 := d2.waitFor("ready", 5*time.Second)
			conn2, _ := ready2["connection_id"].(string)
			if conn2 == "" || conn2 == conn1 {
				t.Fatalf("process 2 connection_id = %q, want distinct from %q", conn2, conn1)
			}

			// Replay process 1's durable log and extract its request ID from there.
			d2.send(map[string]any{
				"id":             "rep1",
				"type":           "replay",
				"after_sequence": 0,
				"connection_id":  conn1,
				"resume_session": sessionID,
			})
			repResp := d2.waitForResponse("rep1", 5*time.Second)
			if repResp["type"] != "response" {
				t.Fatalf("replay response = %v", repResp)
			}
			repData, _ := repResp["data"].(map[string]any)
			if pending, _ := repData["pending_approvals"].([]any); len(pending) != 0 {
				t.Fatalf("durable replay pending_approvals = %v, want empty", pending)
			}
			rawEvents, _ := repData["events"].([]any)
			var replayedID string
			for _, item := range rawEvents {
				ev, _ := item.(map[string]any)
				if ev["type"] == tc.eventType {
					replayedID, _ = ev["id"].(string)
					break
				}
			}
			if replayedID != id1 {
				raw, _ := json.Marshal(repData["events"])
				t.Fatalf("replayed %s id = %q, want %q (events=%s)", tc.eventType, replayedID, id1, raw)
			}

			// Trigger a pending request of the same kind on process 2.
			d2.send(map[string]any{"id": "p2", "type": "prompt", "message": tc.prompt2})
			d2.waitForResponse("p2", 5*time.Second)
			ev2 := d2.waitFor(tc.eventType, 10*time.Second)
			id2, _ := ev2["id"].(string)
			if id2 == "" || id2 == replayedID || !strings.Contains(id2, conn2) {
				t.Fatalf("process 2 %s id = %q, want scoped to %q and distinct from %q", tc.eventType, id2, conn2, replayedID)
			}

			sendReply := func(reqID, targetID, connID string) map[string]any {
				msg := map[string]any{
					"id":       reqID,
					"type":     tc.responseType,
					tc.idField: targetID,
				}
				if connID != "" {
					msg["connection_id"] = connID
				}
				for k, v := range tc.extraFields {
					msg[k] = v
				}
				d2.send(msg)
				return waitForReply(d2, reqID, 5*time.Second)
			}

			// 1. Replayed ID from process 1 (without connection_id) must fail.
			if errResp := sendReply("bad-old-id", replayedID, ""); errResp["type"] != "error" {
				t.Fatalf("replayed id without connection_id succeeded: %v", errResp)
			}
			// 2. Replayed ID from process 1 (with conn1) must fail.
			if errResp := sendReply("bad-old-conn", replayedID, conn1); errResp["type"] != "error" {
				t.Fatalf("replayed id with old connection_id succeeded: %v", errResp)
			}
			// 3. Process 2's ID paired with process 1's connection_id must fail
			//    without consuming process 2's pending waiter.
			if errResp := sendReply("bad-mismatched-conn", id2, conn1); errResp["type"] != "error" {
				t.Fatalf("live id with old connection_id succeeded: %v", errResp)
			}

			// Ensure process 2 is still waiting on its pending request, then
			// resolve it legitimately using id2 and conn2.
			select {
			case ev := <-d2.events:
				if ev["type"] == "agent_settled" {
					t.Fatalf("rejected response resolved process 2 prematurely: %v", ev)
				}
				d2.pending = append(d2.pending, ev)
			case <-time.After(50 * time.Millisecond):
			}

			okResp := sendReply("good", id2, conn2)
			if okResp["type"] != "response" {
				t.Fatalf("valid response failed: %v", okResp)
			}
			settled := d2.waitFor("agent_settled", 10*time.Second)
			tc.verifyAfter(t, root, settled)
			d2.quit(t)
		})
	}
}

func waitForReply(d *driver, id string, timeout time.Duration) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for i, rec := range d.pending {
			if (rec["type"] == "response" || rec["type"] == "error") && rec["id"] == id {
				d.pending = append(d.pending[:i], d.pending[i+1:]...)
				return rec
			}
		}
		rest := time.Until(deadline)
		if rest <= 0 {
			d.t.Fatalf("timed out waiting for reply to %q", id)
		}
		rec := d.next(rest)
		if (rec["type"] == "response" || rec["type"] == "error") && rec["id"] == id {
			return rec
		}
		d.pending = append(d.pending, rec)
	}
}
