package agent

import (
	"testing"

	"minimalharness/internal/llm"
)

func TestToolsForMode(t *testing.T) {
	all := append(ToolDefs(),
		llm.Tool("todo_add", "Add a todo.", `{"type":"object","properties":{"text":{"type":"string"}}}`),
		llm.Tool("plan_create", "Create a plan.", `{"type":"object","properties":{"title":{"type":"string"}}}`),
	)
	names := func(defs []llm.ToolDef) map[string]bool {
		m := map[string]bool{}
		for _, d := range defs {
			m[d.Function.Name] = true
		}
		return m
	}
	def := names(ToolsForMode(ToolModeDefault, all))
	if len(def) != len(all) {
		t.Fatalf("default mode kept %d tools, want %d", len(def), len(all))
	}
	plan := names(ToolsForMode(ToolModePlan, all))
	for _, drop := range []string{"write", "edit", "bash"} {
		if plan[drop] {
			t.Errorf("plan mode still offers %s", drop)
		}
	}
	for _, keep := range []string{"read", "grep", "glob", "load_skill", "webfetch", "todo_add", "plan_create"} {
		if !plan[keep] {
			t.Errorf("plan mode dropped %s, want it kept", keep)
		}
	}
	// ToolsForMode never modifies its input (Setup shares the slice
	// between Cfg.Tools and the stored full list).
	if got := names(all); len(got) != len(all) {
		t.Fatalf("ToolsForMode modified its input")
	}
}

func TestToolModeValid(t *testing.T) {
	if !ToolModeDefault.Valid() || !ToolModePlan.Valid() {
		t.Fatal("default and plan must be valid")
	}
	if ToolMode("review").Valid() || ToolMode("").Valid() {
		t.Fatal("unknown modes must not be valid")
	}
}

func TestReminderFor(t *testing.T) {
	if ReminderFor(ToolModePlan) == "" || ReminderFor(ToolModeDefault) == "" {
		t.Fatal("both modes need a switch reminder")
	}
	if ReminderFor(ToolMode("review")) != "" {
		t.Fatal("unknown mode must have no reminder")
	}
}
