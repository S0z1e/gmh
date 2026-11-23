package agent

import (
	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

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
	// EventThinkingDelta carries one streamed thinking fragment.
	// Text is a delta, like TextDelta. Thinking is display-only:
	// it is never part of the assistant message or the session.
	EventThinkingDelta EventKind = "thinking_delta"
	// EventThinkingEnd carries the full thinking text for one LLM
	// call, so renderers can replace the accumulated deltas with
	// the authoritative text. It is emitted only when the thinking
	// is non-empty; a call with no thinking emits nothing.
	EventThinkingEnd EventKind = "thinking_end"
	EventStepUsage   EventKind = "step_usage"
	EventLLMRequest  EventKind = "llm_request"
	EventToolStart   EventKind = "tool_start"
	EventToolEnd     EventKind = "tool_end"
	// EventApproval fires when the loop pauses for a tool approval.
	// Text carries the approval prompt. The answer itself is visible
	// in the following ToolEnd (denied) or ToolStart/ToolEnd (approved).
	EventApproval EventKind = "approval"
	// EventApprovalReview reports an automatic decision for one invocation.
	EventApprovalReview EventKind = "approval_review"
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
	// EventStopBlocked fires when a stop hook vetoes the turn end.
	// Text is the hook's block reason; the loop feeds it back to
	// the model and continues, up to MaxStopBlocks times.
	EventStopBlocked EventKind = "stop_blocked"
	// EventStepRetry fires before a retry of a failed LLM call.
	// Text says which attempt failed and how long the wait is;
	// RetryAttempt is the attempt number about to run.
	EventStepRetry EventKind = "step_retry"
	// EventStepDiscard fires when a corrupt partial reply is abandoned, or before a retry whose
	// stream was cut off after partial output: it tells every
	// renderer to drop everything streamed for the current step
	// so far — text deltas and thinking deltas — because the
	// stream is being replayed from scratch. Nothing from the
	// discarded attempt enters history or the session. RetryAttempt
	// is the attempt number about to run, or 0 when no retry remains.
	EventStepDiscard EventKind = "step_discard"
)

// TerminalReason is why a turn ended, as a structured value —
// after Qwen Code 1.0's Terminal reason and Codex's typed turn
// errors. A GUI needs to tell "finished" from "stopped by the
// user", "hit the step cap" and "denied at the doom-loop prompt"
// without parsing an error string. The model stop reason (why the
// model stopped emitting) is a different thing and is not this.
type TerminalReason string

const (
	// TerminalEndTurn: the model finished with no more tool calls.
	TerminalEndTurn            TerminalReason = "end_turn"
	TerminalOutputLimit        TerminalReason = "output_limit"
	TerminalBudgetExceeded     TerminalReason = "budget_exceeded"
	TerminalBudgetUnavailable  TerminalReason = "budget_unavailable"
	TerminalStopCap            TerminalReason = "stop_cap"
	TerminalVerificationFailed TerminalReason = "verification_failed"
	// TerminalMaxTurns: the step cap was hit.
	TerminalMaxTurns TerminalReason = "max_turns"
	// TerminalAbort: the turn was aborted (context cancelled).
	TerminalAbort TerminalReason = "abort"
	// TerminalError: the turn failed (LLM / provider error).
	TerminalError TerminalReason = "error"
	// TerminalRepeatedToolCallDenied: the user denied a repeated
	// tool call at the doom-loop continuation prompt, or no prompt
	// was available in a headless run.
	TerminalRepeatedToolCallDenied TerminalReason = "repeated_tool_call_denied"
	// TerminalBlocked: the task was marked blocked after three
	// consecutive failed fix attempts — the model kept trying and
	// failing, so the turn ends instead of looping.
	TerminalBlocked TerminalReason = "blocked"
)

type Event struct {
	StepID string
	Kind   EventKind
	// Text is a streamed delta for TextDelta, the final answer for Done.
	// For Error it carries the turn's last text — the grace summary
	// on a step cap, the model's last message otherwise — so a
	// renderer that only sees the event stream (the RPC server) can
	// still show what the turn produced. Empty when the turn
	// produced no text.
	Text string
	// ToolName / ToolArgs identify the call for ToolStart.
	ToolName   string
	ToolArgs   string
	ToolCallID string
	// InvocationID identifies validated effective args. Early rejections
	// carry only ToolCallID, since no invocation was committed.
	InvocationID string
	// Decision is "approved" or "denied" for EventApprovalReview.
	Decision string
	// ToolOutput is the observation for ToolEnd (including the "Error:"
	// prefix when ToolErr is set).
	ToolOutput     string
	ToolErr        error
	ToolStatus     tools.ResultStatus
	ToolExitCode   *int
	ToolOutputFile string
	// ToolDetail is display-only content for ToolEnd (never sent to
	// the model): write carries the written content, edit a -/+ diff
	// of the replaced lines. Empty for other tools.
	ToolDetail string
	// ToolCount is the number of matches (grep) or files (glob)
	// found, for ToolEnd. Only those two tools set it; other tools
	// leave it 0.
	ToolCount int
	// ToolTruncated marks omitted results; ToolCountLowerBound distinguishes
	// an incomplete search count from an exact total with capped display.
	ToolTruncated       bool
	ToolCountLowerBound bool
	// JobID names the background job for the background events.
	JobID string
	// Usage is the per-call usage for StepUsage, the turn total for Done.
	Usage     llm.Usage
	Request   *llm.RequestTrace
	Operation string
	Attempt   int
	// Err is the failure for EventError.
	Err error
	// TerminalReason says why the turn ended, for Done and Error.
	// Empty for all other events.
	TerminalReason TerminalReason
	TaskOutcome    TaskOutcome
	// RetryAttempt is the attempt number about to run, for
	// EventStepRetry and EventStepDiscard. 0 for all other events.
	RetryAttempt int
}
