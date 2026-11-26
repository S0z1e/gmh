package rpc

import (
	"minimalharness/internal/turnbudget"
	"minimalharness/internal/verification"
	"testing"
	"time"
)

func TestSettledBudgetStopsWithIncompleteOutcome(t *testing.T) {
	for _, approval := range []bool{false, true} {
		t.Run(map[bool]string{false: "tokens", true: "approval_deadline"}[approval], func(t *testing.T) {
			mode := "auto"
			if approval {
				mode = "suggest"
			}
			a, runner, _ := setupTestApp(t, mode, []fakeStep{{kind: "tool", tool: "write", args: map[string]any{"path": "never", "content": "x"}}})
			n := int64(1)
			if approval {
				a.Cfg.TurnBudget = &turnbudget.Policy{MaxDurationSeconds: &n}
			} else {
				a.Cfg.TurnBudget = &turnbudget.Policy{MaxTokens: &n}
			}
			d := startDriver(t, a, runner)
			d.waitFor("ready", 5*time.Second)
			d.send(map[string]any{"id": "p", "type": "prompt", "message": "work"})
			d.waitFor("response", 5*time.Second)
			if approval {
				d.waitFor("approval_request", 5*time.Second)
			}
			settled := d.waitFor("agent_settled", 5*time.Second)
			if settled["reason"] != "budget_exceeded" || settled["outcome"] != "incomplete" || settled["error"] == nil || settled["aborted"] == true {
				t.Fatalf("settled=%v", settled)
			}
			d.quit(t)
		})
	}
}

func TestSettledMissingRatesDoesNotLookLikeAnEnforcedCap(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "an answer"}})
	cost := 1.0
	a.Cfg.TurnBudget = &turnbudget.Policy{MaxCostUSD: &cost, MaxSessionCostUSD: &cost}
	a.Cfg.UsageRates = a.Rates
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p", "type": "prompt", "message": "work"})
	d.waitFor("response", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["reason"] != "end_turn" || settled["outcome"] != nil || settled["error"] != nil {
		t.Fatalf("unenforced cap changed the outcome: %v", settled)
	}
	budget, _ := settled["budget"].(map[string]any)
	if budget["cost_cap_enforced"] != false || budget["session_cost_cap_enforced"] != false || budget["lower_bound"] != true || budget["cost_complete"] != false {
		t.Fatalf("settled budget looked enforced: %v", settled["budget"])
	}
	d.send(map[string]any{"id": "state", "type": "get_state"})
	state := d.waitFor("response", 5*time.Second)
	data, _ := state["data"].(map[string]any)
	label, _ := data["budget"].(map[string]any)
	if label["cost_cap_enforced"] != false || label["lower_bound"] != true {
		t.Fatalf("get_state hid the unenforced cap: %v", state)
	}
	d.quit(t)
}

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
