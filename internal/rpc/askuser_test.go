package rpc

import (
	"strings"
	"testing"
	"time"
)

// ask_user over RPC: the model calls the tool, the GUI gets a
// question_request, answers with a selection, and the turn finishes.
func TestQuestionRequestOverRPC(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "tool", tool: "ask_user", args: map[string]any{"questions": []any{map[string]any{"question": "Which DB?", "options": []any{map[string]any{"label": "SQLite"}, map[string]any{"label": "Postgres"}}}}}},
		{kind: "text", text: "using Postgres"},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Pick a DB"})
	d.waitFor("response", 5*time.Second)
	q := d.waitFor("question_request", 10*time.Second)
	id, _ := q["id"].(string)
	if id == "" {
		t.Fatal("question_request has no id")
	}
	qs, _ := q["questions"].([]any)
	if len(qs) != 1 {
		t.Fatalf("question_request questions = %v", q["questions"])
	}
	d.send(map[string]any{"id": "q1", "type": "question_response", "question_id": id, "answers": []any{map[string]any{"question": 0, "selected": []string{"Postgres"}}}})
	resp := d.waitFor("response", 5*time.Second)
	if resp["type"] != "response" {
		t.Fatalf("question_response reply = %v", resp)
	}
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("settled with error: %v", settled["error"])
	}
	if settled["reason"] != "end_turn" {
		t.Fatalf("settled reason = %v, want end_turn", settled["reason"])
	}
	text, _ := settled["text"].(string)
	if !strings.Contains(text, "Postgres") {
		t.Fatalf("settled text = %q, want it to mention Postgres", text)
	}
	d.quit(t)
	_ = runner
	_ = a
}
