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
	// EventApproval fires when the loop pauses for a tool approval.
	// Text carries the approval prompt. The answer itself is visible
	// in the following ToolEnd (denied) or ToolStart/ToolEnd (approved).
	EventApproval EventKind = "approval"
	// EventToolBackgrounded fires when a bash call runs past the
	// background threshold and keeps running as a job. JobID names it;
	// the tool result delivered to the model says the same.
	EventToolBackgrounded EventKind = "tool_backgrounded"
	// EventBackgroundWait fires once when the turn ends with jobs
	// still running and the loop waits for them. Text lists the job ids.
	EventBackgroundWait EventKind = "background_wait"
	// EventToolBackgroundDone fires when a background job finishes and
	// its result is fed back into the loop as a tool message.
	// ToolOutput is the result; ToolErr the failure, if any.
	EventToolBackgroundDone EventKind = "tool_background_done"
	EventDone               EventKind = "done"
	EventError              EventKind = "error"
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
	// ToolDetail is display-only content for ToolEnd (never sent to
	// the model): write carries the written content, edit a -/+ diff
	// of the replaced lines. Empty for other tools.
	ToolDetail string
	// ToolCount is the number of matches (grep) or files (glob)
	// found, for ToolEnd. Only those two tools set it; other tools
	// leave it 0.
	ToolCount int
	// JobID names the background job for the background events.
	JobID string
	// Usage is the per-call usage for StepUsage, the turn total for Done.
	Usage llm.Usage
	// Err is the failure for EventError.
	Err error
}
