package rpc

import (
	"minimalharness/internal/verification"
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

func TestSettledRequiredVerificationOutcomes(t *testing.T) {
	for _, pass := range []bool{true, false} {
		name := "missing"
		if pass {
			name = "passed"
		}
		t.Run(name, func(t *testing.T) {
			steps := []fakeStep{{kind: "tool", tool: "write", args: map[string]any{"path": "input", "content": "one"}}}
			if pass {
				steps = append(steps, fakeStep{kind: "tool", tool: "bash", args: map[string]any{"command": "true"}})
			}
			steps = append(steps, fakeStep{kind: "text", text: "done"})
			a, runner, _ := setupTestApp(t, "auto", steps)
			a.Cfg.Verification = &verification.Policy{Required: []verification.Check{{Name: "test", Command: "true"}}}
			d := startDriver(t, a, runner)
			d.waitFor("ready", 5*time.Second)
			d.send(map[string]any{"id": "p", "type": "prompt", "message": "work"})
			d.waitFor("response", 5*time.Second)
			settled := d.waitFor("agent_settled", 10*time.Second)
			if pass {
				if settled["reason"] != "end_turn" || settled["outcome"] != "completed" || settled["error"] != nil {
					t.Fatalf("settled=%v", settled)
				}
			} else if settled["reason"] != "verification_failed" || settled["outcome"] != "verification_failed" || settled["error"] == nil {
				t.Fatalf("settled=%v", settled)
			}
			d.quit(t)
		})
	}
}
