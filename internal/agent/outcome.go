package agent

// TaskOutcome is separate from why the model/turn stopped. An empty value
// means no task-level conclusion was established. In particular end_turn does
// not prove completion; only an explicit completion/verification path may do so.
type TaskOutcome string

const (
	OutcomeCompleted          TaskOutcome = "completed"
	OutcomeIncomplete         TaskOutcome = "incomplete"
	OutcomeBlocked            TaskOutcome = "blocked"
	OutcomeAborted            TaskOutcome = "aborted"
	OutcomeVerificationFailed TaskOutcome = "verification_failed"
)

func TaskOutcomeForReason(reason TerminalReason) TaskOutcome {
	switch reason {
	case TerminalAbort:
		return OutcomeAborted
	case TerminalBlocked, TerminalRepeatedToolCallDenied:
		return OutcomeBlocked
	case TerminalError, TerminalOutputLimit, TerminalMaxTurns, TerminalStopCap:
		return OutcomeIncomplete
	default:
		return ""
	}
}
