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
	"minimalharness/internal/textutil"
	"strings"

	"minimalharness/internal/agent"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"minimalharness/internal/turnbudget"
)

// Request is one line from the GUI. ID is echoed in the response when
// present; a request without one still gets a response, without an id.
type Request struct {
	accepted <-chan struct{}
	ID       string `json:"id,omitempty"`
	// Type is one of: prompt, approval_response, continuation_response,
	// question_response, abort, clear_queue, steer_prompt, get_state, new_session,
	// clear, compact, usage, jobs, skill, quit, attach_image, attach_file, list_pending_files,
	// set_model, list_sessions, rename_session, resume_session, steal_session,
	// delete_session, set_tool_mode, set_approval_mode, rollback, list_plugins,
	// list_models, list_skills, history, command.
	Type string `json:"type"`
	// Message is the task for prompt, and the raw args string for
	// command (the text after "/name", unparsed).
	Message string `json:"message,omitempty"`
	// TurnID selects a pending prompt for steer_prompt. For a new
	// prompt, the server assigns its own ID at acceptance.
	TurnID string `json:"turn_id,omitempty"`
	// ActiveTurnID is the running turn that steer_prompt should update.
	ActiveTurnID string `json:"active_turn_id,omitempty"`
	// Name is the skill name for skill, and the command name for
	// command (without the leading "/").
	Name string `json:"name,omitempty"`
	// Path is the user-selected file for attach_image or attach_file.
	Path string `json:"path,omitempty"`
	// Model and Provider select a model for set_model: "provider/model"
	// arrives split (see slashRequest); a bare model id searches the
	// current provider first, then the rest.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Session is the session id for resume_session, steal_session and delete_session.
	Session string `json:"session,omitempty"`
	// Rebind, on steal_session, adopts a legacy transcript or moves the session.
	// The steal request is itself the confirmation; resume_session never steals.
	Rebind bool `json:"rebind,omitempty"`
	// New is the new id for rename_session (Name holds the old one).
	New string `json:"new,omitempty"`
	// Mode selects a tool mode or an approval mode for the corresponding request.
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
	// AfterSequence requests buffered events with Sequence > AfterSequence for replay.
	AfterSequence uint64 `json:"after_sequence,omitempty"`
	// ConnectionID resumes a previous RPC connection's durable replay log.
	// Empty means the current connection.
	ConnectionID string `json:"connection_id,omitempty"`
	// ResumeSession selects the session whose durable replay log to read when
	// it differs from the live session, for example after a process restart.
	ResumeSession string `json:"resume_session,omitempty"`
	// JobID identifies a background job for stop_job.
	JobID string `json:"job_id,omitempty"`
}

// Response is the reply to one request, correlated by ID. Exactly one
// of Data and Error is set: Type is "response" or "error".
type Response struct {
	Sequence uint64 `json:"sequence,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Data     any    `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
}

// UsageJSON is one usage object: prompt/cached/completion/total tokens.
type UsageJSON struct {
	CacheUsageReported        bool   `json:"cache_usage_reported"`
	CacheUsageSource          string `json:"cache_usage_source,omitempty"`
	CacheReportedPromptTokens int    `json:"cache_reported_prompt_tokens"`
	Prompt                    int    `json:"prompt"`
	Cached                    int    `json:"cached"`
	Completion                int    `json:"completion"`
	Total                     int    `json:"total"`
	// Reasoning is the thinking part of the completion tokens: a
	// subset of Completion, never an extra charge. Omitted when the
	// provider did not report it.
	Reasoning int `json:"reasoning,omitempty"`
}

// Event is one line to the GUI. Only the fields for its Type are set;
// the rest are omitted. Types: ready, turn_started, text_delta,
// thinking_delta, thinking_end, step_usage, tool_start, tool_end,
// tool_backgrounded, background_wait, tool_background_done,
// approval_request, prompt_steered, agent_settled, compacted, error.
type Event struct {
	OriginTurnID    string            `json:"origin_turn_id,omitempty"`
	OriginRequestID string            `json:"origin_request_id,omitempty"`
	OriginStepID    string            `json:"origin_step_id,omitempty"`
	Sequence        uint64            `json:"sequence,omitempty"`
	ConnectionID    string            `json:"connection_id,omitempty"`
	TurnID          string            `json:"turn_id,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`
	StepID          string            `json:"step_id,omitempty"`
	Request         *llm.RequestTrace `json:"request,omitempty"`
	Operation       string            `json:"operation,omitempty"`
	Attempt         int               `json:"attempt,omitempty"`
	Type            string            `json:"type"`
	// ready: protocol version and session identity.
	Protocol       int                  `json:"protocol,omitempty"`
	Session        string               `json:"session,omitempty"`
	Model          string               `json:"model,omitempty"`
	API            string               `json:"api,omitempty"`
	Mode           string               `json:"mode,omitempty"`
	Root           string               `json:"root,omitempty"`
	Resumed        bool                 `json:"resumed,omitempty"`
	Messages       int                  `json:"messages,omitempty"`
	ResumedMessage     string                      `json:"resumed_message,omitempty"`
	RecoveredJobs      []tools.RecoveredJob        `json:"recovered_jobs,omitempty"`
	OrphanedJobs       []tools.OrphanedSessionJobs `json:"orphaned_jobs,omitempty"`
	OrphanedJobsNotice string                      `json:"orphaned_jobs_notice,omitempty"`
	// text_delta, background_wait, turn_started: the text.
	Text string `json:"text,omitempty"`
	// step_usage, agent_settled.
	Usage                *UsageJSON           `json:"usage,omitempty"`
	SessionUsage         *UsageJSON           `json:"session_usage,omitempty"`
	RunUsage             *UsageJSON           `json:"current_run_usage,omitempty"`
	Metering             *session.UsageTotals `json:"metering,omitempty"`
	RunUsageComplete     *bool                `json:"current_run_usage_complete,omitempty"`
	SessionUsageComplete *bool                `json:"session_usage_complete,omitempty"`
	MeteringAvailable    *bool                `json:"metering_available,omitempty"`
	// Budget is the turn snapshot. CostCapEnforced false means an active
	// dollar cap was not priced and must not be read as enforced.
	Budget *turnbudget.State `json:"budget,omitempty"`
	// agent_settled cost: null when no rates are configured.
	CostUSD        *float64 `json:"cost_usd,omitempty"`
	SessionCostUSD *float64 `json:"session_cost_usd,omitempty"`
	RunCostUSD     *float64 `json:"current_run_cost_usd,omitempty"`
	Fee            string   `json:"fee,omitempty"`
	// tool_start / tool_end / background events.
	ToolCallID   string `json:"tool_call_id,omitempty"`
	InvocationID string `json:"invocation_id,omitempty"`
	TargetPath   string `json:"target_path,omitempty"`

	Tool       string `json:"tool,omitempty"`
	Status     string `json:"status,omitempty"` // deferred means the tool did not execute
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
	Detail          string `json:"detail,omitempty"`
	Count           int    `json:"count,omitempty"`
	CountLowerBound bool   `json:"count_lower_bound,omitempty"`
	// tool_backgrounded / tool_background_done.
	Job string `json:"job,omitempty"`
	// background_wait: the waiting job ids, comma-separated.
	Jobs string `json:"jobs,omitempty"`
	// approval_request.
	ID     string `json:"id,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// approval_review.
	Decision string `json:"decision,omitempty"`
	// agent_settled.
	Aborted bool `json:"aborted,omitempty"`
	// agent_settled: structured terminal reason — "end_turn",
	// "max_turns", "abort", "error" or "repeated_tool_call_denied"
	// — so the GUI never has to parse an error string to tell how
	// the turn ended. Aborted turns carry reason "abort" and no
	// error string: cancellation is not an error.
	Reason string `json:"reason,omitempty"`
	// outcome is a separate task-level result. Omitted when no completion claim
	// was established; end_turn alone does not mean the task is completed.
	Outcome string `json:"outcome,omitempty"`
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
	if !json.Valid([]byte(argsJSON)) {
		return argsJSON
	}
	var v any
	decoder := json.NewDecoder(strings.NewReader(argsJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil {
		return argsJSON
	}
	return v
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
	return textutil.Prefix(s, head) + marker + textutil.Suffix(s, tail), true
}
