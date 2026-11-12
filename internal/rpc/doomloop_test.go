package rpc

import (
	"strings"
	"testing"
	"time"
)

// The doom-loop guard over RPC: a model that asks for the same read
// five times gets a continuation_request (not an approval_request),
// the GUI denies it, and the turn settles with the structured
// reason repeated_tool_call_denied.
func TestContinuationRequestDeny(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "tool", tool: "read", args: map[string]any{"path": "t.txt"}},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Read t.txt"})
	d.waitFor("response", 5*time.Second)
	cont := d.waitFor("continuation_request", 10*time.Second)
	if cont["tool"] != "read" {
		t.Fatalf("continuation tool = %v, want read", cont["tool"])
	}
	if cont["repeat_count"] != float64(5) {
		t.Fatalf("continuation repeat_count = %v, want 5", cont["repeat_count"])
	}
	id, _ := cont["id"].(string)
	if id == "" {
		t.Fatal("continuation_request has no id")
	}
	d.send(map[string]any{"id": "c1", "type": "continuation_response", "continuation_id": id, "decision": "deny"})
	resp := d.waitFor("response", 5*time.Second)
	if resp["type"] != "response" {
		t.Fatalf("continuation_response reply = %v", resp)
	}
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["reason"] != "repeated_tool_call_denied" {
		t.Fatalf("settled reason = %v, want repeated_tool_call_denied", settled["reason"])
	}
	if settled["aborted"] == true {
		t.Fatalf("settled aborted = true, want false for a denied continuation")
	}
	errStr, _ := settled["error"].(string)
	if !strings.Contains(errStr, "repeated tool call denied") {
		t.Fatalf("settled error = %q, want it to mention the denial", errStr)
	}
	d.quit(t)
}

// The same guard, answered "once": the call runs a fifth time, then
// the streak restarts and trips again after four more repeats —
// answered "session" the second time, the turn finishes.
func TestContinuationRequestOnceThenSession(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "tool", tool: "read", args: map[string]any{"path": "t.txt"}},
	})
	// The fake handler repeats its single step, so the model asks
	// for the same read on every call.
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Read t.txt"})
	d.waitFor("response", 5*time.Second)
	cont := d.waitFor("continuation_request", 10*time.Second)
	id, _ := cont["id"].(string)
	d.send(map[string]any{"id": "c1", "type": "continuation_response", "continuation_id": id, "decision": "once"})
	resp := d.waitFor("response", 5*time.Second)
	if resp["type"] != "response" {
		t.Fatalf("continuation_response reply = %v", resp)
	}
	// After "once" the streak restarts: four more identical calls
	// trip the guard again.
	cont2 := d.waitFor("continuation_request", 10*time.Second)
	id2, _ := cont2["id"].(string)
	if id2 == id {
		t.Fatal("second continuation_request reused the first id")
	}
	d.send(map[string]any{"id": "c2", "type": "continuation_response", "continuation_id": id2, "decision": "session"})
	d.waitFor("response", 5*time.Second)
	// Session-allowed: the call now runs freely until the fake
	// steps run out — the turn ends at the step cap with reason
	// max_turns, not with another continuation prompt.
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["reason"] != "max_turns" {
		t.Fatalf("settled reason = %v, want max_turns after session allow", settled["reason"])
	}
	d.quit(t)
}
