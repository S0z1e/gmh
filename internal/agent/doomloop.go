package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Doom-loop guard, after Qwen Code 1.0 Preview
// (packages/core/src/core/doom-loop-guard.ts and
// tool-executor/streaming/repeated-tool-call.ts).
//
// The model asking for the exact same tool invocation over and over
// is a loop shape, not a permission question, so the guard is
// deliberately NOT routed through the approval system: there is no
// tool check to run and no mode that should skip it. It has its own
// seam (Config.ConfirmContinuation) which shares the UI the approval
// prompts use — the CLI reads a key, the RPC server sends a
// continuation_request event — while the decision path stays separate.
//
// Threshold: 5 consecutive identical calls, like Qwen's default.
// A call is identical when the tool name and the canonical form of
// its arguments match: a different file, a different command, a
// different error all break the streak, so productive repetition
// (polling, retrying with a new argument) is not flagged. A tool
// result that differs from the previous result for the same call
// also breaks the streak (Observe, below): the call is making
// progress, even if its arguments have not changed yet.
//
// On the 5th identical call the loop asks the user once / for the
// session / deny, and the user may supply updated arguments. In a
// headless run there is nobody to ask (ConfirmContinuation is nil,
// or DenyInsteadOfPrompt is set): the guard denies, and the turn
// ends with TerminalRepeatedToolCallDenied — it must never assume
// approval.
//
// ErrRepeatedToolCallDenied is the Run error when the user denies
// (or nobody can be asked). It is a terminal reason, not a crash:
// the turn ends, the session stays usable.

const (
	// RepeatedToolCallThreshold is the consecutive identical calls
	// that trip the guard.
	RepeatedToolCallThreshold = 5
	// SessionAllowedKeyLimit caps the per-session allowlist, like
	// Qwen's 128.
	SessionAllowedKeyLimit = 128
)

// ErrRepeatedToolCallDenied is returned by Run when a repeated tool
// call is denied at the continuation prompt (or no prompt is
// available in a headless run).
var ErrRepeatedToolCallDenied = errors.New("repeated tool call denied")

// ContinuationRequest is what the guard asks the user to confirm.
type ContinuationRequest struct {
	ToolName    string
	RepeatCount int
	Threshold   int
	// InputPreview is the canonical arguments, capped for display.
	InputPreview string
}

// ContinuationOutcome is how the user answered: "once", "session"
// or "deny".
type ContinuationOutcome string

const (
	ContinuationOnce    ContinuationOutcome = "once"
	ContinuationSession ContinuationOutcome = "session"
	ContinuationDeny    ContinuationOutcome = "deny"
)

// ContinuationDecision is the answer to a ContinuationRequest.
// UpdatedArgs, when non-empty, replaces the repeated call's
// arguments (the user edited the input, as in Qwen's
// updatedInput). It must be a JSON object string.
type ContinuationDecision struct {
	Outcome     ContinuationOutcome
	UpdatedArgs string
}

// RepeatedToolCallDetection is the guard's report when a call trips
// it.
type RepeatedToolCallDetection struct {
	ToolName     string
	InputKey     string
	RepeatCount  int
	Threshold    int
	InputPreview string
}

// DoomLoopGuard tracks one streak of identical tool calls plus the
// per-session allowlist. It is not safe for concurrent use; the
// agent loop calls it from its single goroutine.
type DoomLoopGuard struct {
	threshold int
	lastKey   string
	count     int
	// lastObs is the observation of the most recent execution of
	// lastKey, used by Observe to tell progress from a loop.
	lastObs string
	hasObs  bool
	allowed map[string]struct{}
	order   []string
}

func NewDoomLoopGuard(threshold int) *DoomLoopGuard {
	if threshold <= 0 {
		threshold = RepeatedToolCallThreshold
	}
	return &DoomLoopGuard{threshold: threshold, allowed: map[string]struct{}{}}
}

// CanonicalToolKey is the identity of one tool invocation: the tool
// name plus the canonical (key-sorted) form of its arguments JSON.
// Non-JSON arguments fall back to the raw string, so they still get
// a stable key.
func CanonicalToolKey(toolName, argsJSON string) string {
	var v any
	if err := json.Unmarshal([]byte(argsJSON), &v); err != nil {
		return toolName + ":" + argsJSON
	}
	return toolName + ":" + canonicalJSON(v)
}

// canonicalJSON renders v with object keys sorted, so two argument
// objects that differ only in key order get the same key.
// encoding/json already sorts map keys; the recursion only exists
// to keep the shape explicit and to normalise numbers.
func canonicalJSON(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			b.WriteString(canonicalJSON(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(canonicalJSON(e))
		}
		b.WriteByte(']')
		return b.String()
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// Check records one requested call. It returns detected=true with a
// detection when the call is the threshold-th identical consecutive
// one; otherwise the call may run. A session-allowed key never
// trips, and clears the streak.
func (g *DoomLoopGuard) Check(toolName, argsJSON string) (detected bool, det *RepeatedToolCallDetection) {
	key := CanonicalToolKey(toolName, argsJSON)
	if _, ok := g.allowed[key]; ok {
		g.clearStreak()
		return false, nil
	}
	count := 1
	if key == g.lastKey {
		count = g.count + 1
	}
	g.lastKey = key
	g.count = count
	// A new key invalidates the stored observation.
	if count == 1 {
		g.hasObs = false
		g.lastObs = ""
	}
	if count < g.threshold {
		return false, nil
	}
	preview := argsJSON
	if len(preview) > 500 {
		preview = preview[:497] + "..."
	}
	return true, &RepeatedToolCallDetection{
		ToolName: toolName, InputKey: key, RepeatCount: count,
		Threshold: g.threshold, InputPreview: preview,
	}
}

// AllowOnce lets this exact call run one more time, then the streak
// restarts from 1: it takes threshold-1 further repeats to trip
// again.
func (g *DoomLoopGuard) AllowOnce(key string) {
	g.lastKey = key
	g.count = 1
	g.hasObs = false
	g.lastObs = ""
}

// AllowSession adds key to the session allowlist (oldest evicted
// past the limit) and clears the streak, so the call never trips
// again in this session.
func (g *DoomLoopGuard) AllowSession(key string) {
	if _, ok := g.allowed[key]; !ok {
		g.allowed[key] = struct{}{}
		g.order = append(g.order, key)
		for len(g.order) > SessionAllowedKeyLimit {
			delete(g.allowed, g.order[0])
			g.order = g.order[1:]
		}
	}
	g.clearStreak()
}

// Observe records a finished execution's observation. When the
// observation differs from the previous one for the same call, the
// call is making progress and the streak is cleared — this is
// Qwen's repeatNeutral, generalised: a polling call whose output
// changes is not a doom loop.
func (g *DoomLoopGuard) Observe(key, obs string) {
	if key != g.lastKey {
		return
	}
	if g.hasObs && obs != g.lastObs {
		g.clearStreak()
		return
	}
	g.lastObs = obs
	g.hasObs = true
}

func (g *DoomLoopGuard) clearStreak() {
	g.lastKey = ""
	g.count = 0
	g.hasObs = false
	g.lastObs = ""
}

// RepeatedToolCallMessage is the observation / error text when a
// repeated call is denied.
func RepeatedToolCallMessage(toolName string, repeatCount int) string {
	return fmt.Sprintf("Repeated tool call detected: %s was requested %d times with identical input and the continuation was denied. Do not retry this tool call; explain what is stuck and propose a different approach.", toolName, repeatCount)
}
