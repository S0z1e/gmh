package rpc

import (
	"testing"
	"time"
)

func TestSettledStepCapCarriesIncompleteOutcome(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "tool", tool: "read", args: map[string]any{"path": "missing"}},
		{kind: "text", text: "still unfinished"},
	})
	a.Cfg.MaxSteps = 1
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p", "type": "prompt", "message": "do work"})
	d.waitFor("response", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["reason"] != "max_turns" || settled["outcome"] != "incomplete" || settled["error"] == nil {
		t.Fatalf("settled=%v", settled)
	}
	d.quit(t)
}

func TestSettledEndTurnDoesNotAssertCompletion(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "an answer"}})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p", "type": "prompt", "message": "question"})
	d.waitFor("response", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["reason"] != "end_turn" || settled["outcome"] != nil {
		t.Fatalf("end_turn claimed task completion: %v", settled)
	}
	d.quit(t)
}
