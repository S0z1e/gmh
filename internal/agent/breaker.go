package agent

import (
	"errors"
	"fmt"
)

// Repeat-failure circuit breaker, after Qoder's denial circuit
// breaker (3 consecutive / 20 cumulative failures) — adapted: this
// one counts failures per fingerprint within one turn, not
// consecutive repeats.
//
// The doom-loop guard catches a call repeated 5 times in a row; it
// cannot see a call that keeps FAILING with something else in
// between. The bookstore run ran bare `npm run build` 7 times with
// the identical "Missing script: build" error, interleaved with
// reads and edits, and the consecutive guard never fired. The
// breaker counts failures per fingerprint (tool name + canonical
// arguments, the doom-loop guard's key): the 2nd failure warns in
// the observation, and the 4th attempt is denied before it runs.
// A success clears the fingerprint's count, and counts reset every
// turn (a new breaker per Run). A denial never executes the command
// and does not increment the count.
//
// The denial is NOT the doom-loop guard's `repeated_tool_call_denied`:
// that one means the continuation was refused at the prompt; this
// one means the call already failed too many times this turn. It is
// delivered as a tool error result, not a turn end: the model gets
// the denial and can try something else. For step accounting it
// counts as a failed fix attempt (the model did try to fix
// something; the harness refused on evidence of prior failure),
// unlike a hook, approval or not-offered denial, which never
// executed — so three consecutive denied-or-failed steps still end
// the turn `blocked`, with the blocked-grace summary.

const (
	// FailureWarnAfter is the failure count at which the
	// observation gains a warning that the next identical call
	// will be refused.
	FailureWarnAfter = 2
	// FailureDenyAfter is the failure count at which the next
	// identical attempt is denied before executing.
	FailureDenyAfter = 3
)

// RepeatedToolFailureReason is the structured reason for a breaker
// denial, carried in the tool error result.
const RepeatedToolFailureReason = "repeated_tool_failure_denied"

// ErrRepeatedToolFailureDenied is the tool error for a breaker
// denial (the observation carries the full text).
var ErrRepeatedToolFailureDenied = errors.New("repeated tool failure denied")

// FailureBreaker counts failed tool calls per fingerprint within
// one turn. It is not safe for concurrent use; the agent loop
// calls it from its single goroutine.
type FailureBreaker struct {
	counts map[string]int
}

// NewFailureBreaker returns a breaker with no counts: one per Run.
func NewFailureBreaker() *FailureBreaker {
	return &FailureBreaker{counts: map[string]int{}}
}

// Check reports whether the next attempt of this fingerprint must
// be denied, and how many times it has already failed this turn.
func (b *FailureBreaker) Check(key string) (denied bool, failures int) {
	failures = b.counts[key]
	return failures >= FailureDenyAfter, failures
}

// ObserveFailure records one failure and returns the new count.
func (b *FailureBreaker) ObserveFailure(key string) int {
	b.counts[key]++
	return b.counts[key]
}

// ObserveSuccess clears a fingerprint's failure count: a call that
// works is not a failure loop, even if it failed before.
func (b *FailureBreaker) ObserveSuccess(key string) {
	delete(b.counts, key)
}

// FailureNote is the warning appended to a failure observation
// once the count reaches FailureWarnAfter.
func FailureNote(toolName string, failures int) string {
	if failures >= FailureDenyAfter {
		return fmt.Sprintf("Note: this exact %s call has already failed %d times this turn. The next identical call will be refused — change the approach instead of repeating it.", toolName, failures)
	}
	return fmt.Sprintf("Note: this exact %s call has already failed %d times this turn. One more failure and the next identical call will be refused — change the approach instead of repeating it.", toolName, failures)
}

// RepeatedFailureDeniedMessage is the tool error observation when
// the breaker denies an attempt.
func RepeatedFailureDeniedMessage(toolName string, failures int) string {
	return fmt.Sprintf("Error: repeated failure denied (%s): %s with these exact arguments has already failed %d times this turn. Do not retry it; explain what is stuck and propose a different approach.", RepeatedToolFailureReason, toolName, failures)
}
