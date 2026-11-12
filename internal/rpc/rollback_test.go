package rpc

import (
	"strings"
	"testing"
	"time"
)

// thread/rollback over RPC: two turns run, rollback 1 drops the
// second turn, and history shows only the first.
func TestRollbackDropsLastTurn(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "first answer"},
		{kind: "text", text: "second answer"},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	for _, msg := range []string{"first task", "second task"} {
		d.send(map[string]any{"id": "p-" + msg, "type": "prompt", "message": msg})
		d.waitFor("response", 5*time.Second)
		d.waitFor("agent_settled", 10*time.Second)
	}
	if got := len(a.History); got != 4 {
		t.Fatalf("history before rollback = %d messages, want 4", got)
	}
	d.send(map[string]any{"id": "rb1", "type": "rollback", "count": 1})
	resp := d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	if data["dropped"] != float64(2) || data["messages"] != float64(2) {
		t.Fatalf("rollback response = %v, want dropped 2, messages 2", resp)
	}
	if got := len(a.History); got != 2 {
		t.Fatalf("history after rollback = %d messages, want 2", got)
	}
	if a.History[0].Content != "first task" || strings.TrimSpace(a.History[1].Content) != "first answer" {
		t.Fatalf("kept history = %+v, want the first turn only", a.History)
	}
	// Rolling back more turns than exist drops everything left.
	d.send(map[string]any{"id": "rb2", "type": "rollback", "count": 5})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["dropped"] != float64(2) || data["messages"] != float64(0) {
		t.Fatalf("over-rollback response = %v, want dropped 2, messages 0", resp)
	}
	// Rolling back with nothing left is an error, not a silent no-op.
	// (Error replies carry type "error", not "response".)
	d.send(map[string]any{"id": "rb3", "type": "rollback", "count": 1})
	resp = d.waitFor("error", 5*time.Second)
	if resp["id"] != "rb3" {
		t.Fatalf("empty rollback reply = %v, want the rb3 error", resp)
	}
	d.quit(t)
	_ = runner
}
