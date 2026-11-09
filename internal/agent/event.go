package agent

import "minimalharness/internal/llm"

// Event stream seam (P1-3a). The loop emits events; the CLI is one
// renderer of them. A future Bubble Tea TUI will be another renderer
// consuming the same stream — the loop never imports any UI code and
// -p mode never imports the TUI.
//
// Rendering rule: TextDelta carries streamed text as it arrives.
// Everything else (tool calls, usage, done, error) is a boundary at
// which a renderer flushes any pending line.
type EventKind string

const (
	EventTextDelta EventKind = "text_delta"
	EventStepUsage EventKind = "step_usage"
	EventToolStart EventKind = "tool_start"
	EventToolEnd   EventKind = "tool_end"
	EventDone      EventKind = "done"
	EventError     EventKind = "error"
)

type Event struct {
	Kind EventKind
	// Text is a streamed delta for TextDelta, the final answer for Done.
	Text string
	// ToolName / ToolArgs identify the call for ToolStart.
	ToolName string
	ToolArgs string
	// ToolOutput is the observation for ToolEnd (including the "Error:"
	// prefix when ToolErr is set).
	ToolOutput string
	ToolErr    error
	// Usage is the per-call usage for StepUsage, the turn total for Done.
	Usage llm.Usage
	// Err is the failure for EventError.
	Err error
}
