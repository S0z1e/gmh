// Package rpc speaks JSONL over stdio for a GUI: one JSON object per
// line on stdin (requests) and stdout (responses and events), LF
// terminated, split on LF only. Pi's rpc mode does the same, and the
// MyGo demo client already speaks it, so a GUI written against Pi's
// protocol needs only small changes to talk to this harness.
// Diagnostics go to stderr; stdout carries protocol data only.
package rpc

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is one line from the GUI. ID is echoed in the response when
// present; a request without one still gets a response, without an id.
type Request struct {
	ID string `json:"id,omitempty"`
	// Type is one of: prompt, approval_response, abort, clear_queue,
	// get_state, new_session, clear, compact, usage, jobs, skill,
	// quit, attach_image, set_model, list_sessions, rename_session,
	// resume_session.
	Type string `json:"type"`
	// Message is the task for prompt.
	Message string `json:"message,omitempty"`
	// Name is the skill name for skill.
	Name string `json:"name,omitempty"`
	// Path is the image file for attach_image.
	Path string `json:"path,omitempty"`
	// Model and Provider select a model for set_model: "provider/model"
	// arrives split (see slashRequest); a bare model id searches the
	// current provider first, then the rest.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Session is the session id for resume_session.
	Session string `json:"session,omitempty"`
	// New is the new id for rename_session (Name holds the old one).
	New string `json:"new,omitempty"`
	// ApprovalID names the approval_request being answered, for
	// approval_response. It is separate from ID, which correlates
	// the response to this request.
	ApprovalID string `json:"approval_id,omitempty"`
	// Approved answers an approval_request, for approval_response.
	Approved bool `json:"approved,omitempty"`
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
}

// Event is one line to the GUI. Only the fields for its Type are set;
// the rest are omitted. Types: ready, turn_started, text_delta,
// step_usage, tool_start, tool_end, tool_backgrounded,
// background_wait, tool_background_done, approval_request,
// agent_settled, error.
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
	Tool      string `json:"tool,omitempty"`
	Args      any    `json:"args,omitempty"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
	// tool_backgrounded / tool_background_done.
	Job string `json:"job,omitempty"`
	// background_wait: the waiting job ids, comma-separated.
	Jobs string `json:"jobs,omitempty"`
	// approval_request.
	ID     string `json:"id,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// agent_settled.
	Aborted bool `json:"aborted,omitempty"`
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
