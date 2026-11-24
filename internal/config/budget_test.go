package config

import (
	"encoding/json"
	"testing"
)

func TestTurnBudgetMergeAndValidation(t *testing.T) {
	var global, project File
	if err := json.Unmarshal([]byte(`{"turnBudget":{"maxToolCalls":12,"maxTokens":500}}`), &global); err != nil {
		t.Fatal(err)
	}
	Merge(&global, &project)
	if *global.TurnBudget.MaxToolCalls != 12 {
		t.Fatal("omitted policy replaced global budget")
	}
	if err := json.Unmarshal([]byte(`{"turnBudget":{"maxToolCalls":0}}`), &project); err != nil {
		t.Fatal(err)
	}
	Merge(&global, &project)
	*project.TurnBudget.MaxToolCalls = 99
	limits, err := global.TurnBudget.Resolve()
	if err != nil || limits.ToolCalls != 0 || limits.Tokens != 2_000_000 || limits.SessionTokens != 10_000_000 || limits.ConcurrentJobs != 8 {
		t.Fatalf("whole policy replacement/clone failed: %+v %v", limits, err)
	}
	for _, raw := range []string{`{"maxTokens":-1}`, `{"maxCostUSD":-1}`, `{"maxDurationSeconds":9223372036854775807}`} {
		var f File
		if err := json.Unmarshal([]byte(`{"turnBudget":`+raw+`}`), &f); err != nil {
			t.Fatal(err)
		}
		if Validate(&f) == nil {
			t.Fatal("accepted invalid budget:", raw)
		}
	}
}
