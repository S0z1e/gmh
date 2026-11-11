package agent

import (
	"minimalharness/internal/llm"
)

// ToolMode is the loop's active tool set. It is NOT Config.Mode, the
// approval mode ("suggest" / "auto"): a tool mode says which tools
// the model is offered, an approval mode says whether write/edit/bash
// ask first. The two are independent — plan mode still asks, in the
// modes where anything needs asking at all.
type ToolMode string

const (
	// ToolModeDefault offers every tool: the eight built-ins plus
	// all plugin tools.
	ToolModeDefault ToolMode = "default"
	// ToolModePlan is read-only exploration: the workspace-writing
	// tools are removed, so the model can only look and write a plan.
	ToolModePlan ToolMode = "plan"
)

// Valid reports whether m is a mode the core defines. Plugins switch
// between defined modes; they never define a new one.
func (m ToolMode) Valid() bool {
	return m == ToolModeDefault || m == ToolModePlan
}

// planDroppedTools are the built-in tools plan mode removes: the
// ones that write outside a plugin's jailed data dir. A plugin tool
// can never be one of these names (registration rejects collisions
// with built-ins), so filtering by name is exact.
var planDroppedTools = map[string]bool{
	"write": true,
	"edit":  true,
	"bash":  true,
}

// ToolsForMode returns the tools offered in mode m, as a new slice
// (all is never modified). Plan mode drops write/edit/bash; plugin
// tools are always kept — they run JS only, jailed to their own data
// dir, and the plan plugin needs to write its state there.
func ToolsForMode(m ToolMode, all []llm.ToolDef) []llm.ToolDef {
	if m != ToolModePlan {
		out := make([]llm.ToolDef, len(all))
		copy(out, all)
		return out
	}
	out := make([]llm.ToolDef, 0, len(all))
	for _, d := range all {
		if planDroppedTools[d.Function.Name] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Mode reminders, injected as a one-turn system message on a mode
// switch (never persisted to the session): the model needs to be
// told why write disappeared, or it will try to call a tool that is
// no longer offered and waste the turn. The plan reminder names the
// removed tools explicitly: the system prompt still lists
// write/edit/bash (it is built once at Setup), so a bare "read-only"
// does not tell the model the tools are gone — the live e2e showed
// it listing them anyway. The reminder is the per-turn correction.
const (
	// PlanModeReminder is injected when entering plan mode.
	PlanModeReminder = "You are in plan mode: read-only. The write, edit and bash tools are not available in this turn — do not call them. Explore and write a plan with the plan tools; do not write or edit files."
	// DefaultModeReminder is injected when leaving plan mode.
	DefaultModeReminder = "Back in default mode: you may write, edit and run bash again."
)

// ReminderFor returns the one-turn system reminder for entering
// mode m ("" when m is the default mode and no switch happened —
// callers set it only on an actual switch).
func ReminderFor(m ToolMode) string {
	switch m {
	case ToolModePlan:
		return PlanModeReminder
	case ToolModeDefault:
		return DefaultModeReminder
	default:
		return ""
	}
}
