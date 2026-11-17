package agent

import (
	"errors"
	"fmt"

	"minimalharness/internal/failureguard"
)

// Failure admission is owned by failureguard. These messages explain its
// per-invocation evidence to the model, separately from the doom-loop prompt.

const (
	// FailureWarnAfter is the failure count at which the
	// observation gains a warning that the next identical call
	// will be refused.
	FailureWarnAfter = 2
	// FailureDenyAfter is the failure count at which the next
	// identical attempt is denied before executing.
	FailureDenyAfter = failureguard.DenyAfter
)

// RepeatedToolFailureReason is the structured reason for a breaker
// denial, carried in the tool error result.
const RepeatedToolFailureReason = "repeated_tool_failure_denied"

// ErrRepeatedToolFailureDenied is the tool error for a breaker
// denial (the observation carries the full text).
var ErrRepeatedToolFailureDenied = errors.New("repeated tool failure denied")

// FailureNote is the warning appended to a failure observation
// once the count reaches FailureWarnAfter.
func FailureNote(toolName string, failures int) string {
	if failures >= FailureDenyAfter {
		return fmt.Sprintf("Note: this exact %s call has already failed %d times this turn. The next identical call will be refused unless proven relevant state has changed and a bounded probe remains. Change the approach instead of repeating it.", toolName, failures)
	}
	return fmt.Sprintf("Note: this exact %s call has already failed %d times this turn. One more failure and the next identical call will be refused — change the approach instead of repeating it.", toolName, failures)
}

// RepeatedFailureDeniedMessage is the tool error observation when
// the breaker denies an attempt.
func RepeatedFailureDeniedMessage(toolName string, failures int) string {
	return fmt.Sprintf("Error: repeated failure denied (%s): %s with these exact arguments has already failed %d times this turn. A retry requires proven changed state and a remaining probe allowance. Do not repeat an unchanged call; explain what is stuck and propose a different approach.", RepeatedToolFailureReason, toolName, failures)
}
