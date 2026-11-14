// Package rpc speaks JSONL over stdio for a GUI: one JSON object per
// line on stdin (requests) and stdout (responses and events), LF
// terminated, split on LF only. The MyGo demo client already speaks it,
// so a GUI written against this protocol needs only small changes to
// talk to this harness.
// Diagnostics go to stderr; stdout carries protocol data only.
package rpc

import (
	"encoding/json"
	"fmt"
	"strings"

	"minimalharness/internal/agent"
)

// Request is one line from the GUI. ID is echoed in the response when
// present; a request without one still gets a response, without an id.
type Request struct {
	ID string `json:"id,omitempty"`
	// Type is one of: prompt, approval_response, continuation_response,
	// question_response, abort, clear_queue, get_state, new_session,
	// clear, compact, usage, jobs, skill, quit, attach_image,
	// set_model, list_sessions, rename_session, resume_session,
	// delete_session, set_tool_mode, rollback, list_plugins,
	// list_models, list_skills, history, command.
	Type string `json:"type"`
	// Message is the task for prompt, and the raw args string for
	// command (the text after "/name", unparsed).
	Message string `json:"message,omitempty"`
	// Name is the skill name for skill, and the command name for
	// command (without the leading "/").
	Name string `json:"name,omitempty"`
	// Path is the image file for attach_image.
	Path string `json:"path,omitempty"`
	// Model and Provider select a model for set_model: "provider/model"
	// arrives split (see slashRequest); a bare model id searches the
	// current provider first, then the rest.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Session is the session id for resume_session and delete_session.
	Session string `json:"session,omitempty"`
	// New is the new id for rename_session (Name holds the old one).
	New string `json:"new,omitempty"`
	// Mode is the tool mode for set_tool_mode ("default" or "plan").
	Mode string `json:"mode,omitempty"`
	// ApprovalID names the approval_request being answered, for
	// approval_response. It is separate from ID, which correlates
	// the response to this request.
	ApprovalID string `json:"approval_id,omitempty"`
	// Approved answers an approval_request, for approval_response.
	Approved bool `json:"approved,omitempty"`
	// ContinuationID names the continuation_request being answered,
	// for continuation_response (the doom-loop guard's prompt, which
	// is deliberately separate from approval_response).
	ContinuationID string `json:"continuation_id,omitempty"`
	// Decision answers a continuation_request: "once", "session" or
	// "deny".
	Decision string `json:"decision,omitempty"`
	// UpdatedArgs is the edited arguments JSON for a
	// continuation_response, when the GUI let the user edit the
	// repeated call's input.
	UpdatedArgs string `json:"updated_args,omitempty"`
	// Count is the turn count for rollback (default 1 when omitted).
	Count int `json:"count,omitempty"`
	// QuestionID names the question_request being answered, for
	// question_response (the ask_user tool's structured questions).
	QuestionID string `json:"question_id,omitempty"`
	// Answers answers a question_request, one per question.
	Answers []agent.QuestionAnswer `json:"answers,omitempty"`
}

// Response is the reply to one request, correlated by ID. Exactly one
// of Data and Error is set: Type is "response" or "error".
type Response struct {
	ID    string `json:"id,omitempty"`
	Type  string `json:"type"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// UsageJSON is one usage object: prompt/cached/completion/total tokens.
type UsageJSON struct {
	Prompt     int `json:"prompt"`
	Cached     int `json:"cached"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
	// Reasoning is the thinking part of the completion tokens: a
	// subset of Completion, never an extra charge. Omitted when the
	// provider did not report it.
	Reasoning int `json:"reasoning,omitempty"`
}

// Event is one line to the GUI. Only the fields for its Type are set;
// the rest are omitted. Types: ready, turn_started, text_delta,
// thinking_delta, thinking_end, step_usage, tool_start, tool_end,
// tool_backgrounded, background_wait, tool_background_done,
// approval_request, agent_settled, compacted, error.
type Event struct {
	Type string `json:"type"`
	// ready: protocol version and session identity.
	Protocol int    `json:"protocol,omitempty"`
	Session  string `json:"session,omitempty"`
	Model    string `json:"model,omitempty"`
	API      string `json:"api,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Root     string `json:"root,omitempty"`
	Resumed  bool   `json:"resumed,omitempty"`
	Messages int    `json:"messages,omitempty"`
	// text_delta, background_wait, turn_started: the text.
	Text string `json:"text,omitempty"`
	// step_usage, agent_settled.
	Usage        *UsageJSON `json:"usage,omitempty"`
	SessionUsage *UsageJSON `json:"session_usage,omitempty"`
	// agent_settled cost: null when no rates are configured.
	CostUSD        *float64 `json:"cost_usd,omitempty"`
	SessionCostUSD *float64 `json:"session_cost_usd,omitempty"`
	Fee            string   `json:"fee,omitempty"`
	// tool_start / tool_end / background events.
	Tool       string `json:"tool,omitempty"`
	Status     string `json:"status,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	OutputFile string `json:"output_file,omitempty"`
	Args       any    `json:"args,omitempty"`
	Output     string `json:"output,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Error      string `json:"error,omitempty"`
	// tool_end only: display detail (write content / edit diff) and,
	// for grep/glob, the match count (0 omitted; the output text
	// says "no matches" then). The GUI renders these; the model
	// never sees them.
	Detail string `json:"detail,omitempty"`
	Count  int    `json:"count,omitempty"`
	// tool_backgrounded / tool_background_done.
	Job string `json:"job,omitempty"`
	// background_wait: the waiting job ids, comma-separated.
	Jobs string `json:"jobs,omitempty"`
	// approval_request.
	ID     string `json:"id,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// agent_settled.
	Aborted bool `json:"aborted,omitempty"`
	// agent_settled: structured terminal reason — "end_turn",
	// "max_turns", "abort", "error" or "repeated_tool_call_denied"
	// — so the GUI never has to parse an error string to tell how
	// the turn ended. Aborted turns carry reason "abort" and no
	// error string: cancellation is not an error.
	Reason string `json:"reason,omitempty"`
	// continuation_request: how many identical calls were seen and
	// the threshold that tripped the guard.
	RepeatCount int `json:"repeat_count,omitempty"`
	Threshold   int `json:"threshold,omitempty"`
	// question_request: the ask_user tool's questions, in the
	// agent.QuestionRequest shape.
	Questions any `json:"questions,omitempty"`
	// ToolMode is the loop's tool mode ("default" or "plan") on
	// agent_settled, so the GUI knows which tools the next turn
	// offers without polling get_state.
	ToolMode string `json:"tool_mode,omitempty"`
	// compacted: the summary's token count (the completion tokens of
	// the compact call, which generated the summary). Messages
	// carries the new message count after the compact.
	SummaryTokens int `json:"summary_tokens,omitempty"`
	// get_state answers (also carried in the response data, not events).
	Streaming   bool `json:"streaming,omitempty"`
	Queued      int  `json:"queued,omitempty"`
	Turns       int  `json:"turns,omitempty"`
	TotalTokens int  `json:"total_tokens,omitempty"`
}

// encodeLine marshals v as one JSON object plus LF.
func encodeLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// decodeRequest parses one LF-terminated line into a Request. Empty
// lines are the caller's to skip; a line that is not JSON is an error.
func decodeRequest(line string) (Request, error) {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	var req Request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return req, err
	}
	if req.Type == "" {
		return req, fmt.Errorf("request has no type")
	}
	return req, nil
}

// parseToolArgs decodes a tool-call arguments JSON string for an event.
// It returns the parsed object; when the args do not parse, the raw
// string, so the GUI still sees what was asked.
func parseToolArgs(argsJSON string) any {
	var v any
	if err := json.Unmarshal([]byte(argsJSON), &v); err != nil {
		return argsJSON
	}
	return v
}

// parseApprovalPrompt splits an approval prompt "Run <tool> <json>?"
// into the tool name and its args. It falls back to the raw prompt
// when the shape is unexpected, so the dialog never hides what it is
// asking about. (Same shape as the TUI's parser; the rpc package
// stays stdlib-only, so it carries its own copy.)
func parseApprovalPrompt(prompt string) (tool string, args any) {
	rest := strings.TrimSuffix(strings.TrimPrefix(prompt, "Run "), "?")
	i := strings.Index(rest, " ")
	if i < 0 {
		return "", prompt
	}
	tool, argsJSON := rest[:i], rest[i+1:]
	return tool, parseToolArgs(argsJSON)
}

// capOutput caps s at maxChars with a head+tail cut, like the loop's
// truncation of tool results for the LLM view: a GUI must never be
// flooded by one huge read. It returns the capped text and whether
// anything was cut.
func capOutput(s string, maxChars int) (string, bool) {
	if len(s) <= maxChars {
		return s, false
	}
	head := maxChars / 2
	tail := maxChars - head
	marker := fmt.Sprintf("\n[truncated: %d chars shown of %d]", maxChars, len(s))
	return s[:head] + marker + s[len(s)-tail:], true
}
