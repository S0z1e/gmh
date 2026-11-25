// Package rpc's server: one harness process speaking JSONL over stdio.
//
// Concurrency is deliberately boring: a reader goroutine decodes stdin
// and answers read-only queries (get_state, usage, jobs) immediately;
// everything that touches history or the session goes through one work
// queue, and a single agent goroutine runs one item at a time. The
// reader resolves dialogs, cancels turns, and handles queue edits
// immediately, even while the agent goroutine is busy.
//
// stdout carries protocol data only; diagnostics go to stderr.
package rpc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/approvalmode"
	"minimalharness/internal/config"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

const (
	protocolVersion    = 1
	maxQueue           = 64
	maxToolOutputChars = 32 * 1024
	maxReplayEvents    = 2048
	maxReplayBytes     = 2 * maxOutputFrameBytes
	maxReplayEventSize = maxOutputFrameBytes - 4096 // leave room for the response envelope
	defaultDetachGrace = 5 * time.Second
)

var (
	errAborted  = errors.New("turn aborted")
	errShutdown = errors.New("rpc server is shutting down")
)

// serverState is the read-only snapshot a GUI polls with get_state. It
// is refreshed by the agent goroutine after every work item, so the
// reader goroutine can answer without touching runner state.
type serverState struct {
	workspaceTrusted            bool
	streaming                   bool
	turns                       int
	sessionUsage                llm.Usage
	runUsage                    llm.Usage
	runCost                     float64
	runCostKnown                bool
	meteringAvailable           bool
	sessionUsageComplete        bool
	runUsageComplete            bool
	metering                    session.UsageTotals
	sessionCost                 float64
	messages                    int
	sessionID                   string
	model, api, mode, root, fee string
	contextWindow, maxTokens    int
	ratesConfigured             bool
	// toolMode is the loop's tool mode ("default" or "plan").
	toolMode string
}

type eventOrigin struct{ turnID, requestID, stepID string }

type turnOutcome struct {
	text    string
	usage   llm.Usage
	err     error
	reason  agent.TerminalReason
	outcome agent.TaskOutcome
}

type approvalAnswer struct {
	approved bool
	err      error
}

type pendingApproval struct {
	ch  chan approvalAnswer
	req agent.ApprovalRequest
}

// questionAnswer is one question_request's reply, or the abort that
// cancelled it.
type questionAnswer struct {
	ans agent.QuestionAnswers
	err error
}

type workKind int

const (
	workPrompt workKind = iota
	workControl
)

type workItem struct {
	kind workKind
	req  Request
}

// Server owns one harness process's RPC conversation.
type Server struct {
	app    *app.App
	runner *app.Runner
	in     io.Reader
	out    io.Writer

	mu           sync.Mutex // guards state, never output I/O
	writeMu      sync.Mutex // orders frames, independent from state
	output       *protocolOutput
	sequence     uint64
	connectionID string
	eventHistory []replayEvent // replay history and its size, guarded by writeMu
	replayBytes  int
	evictedSeq   uint64 // highest event sequence evicted from eventHistory, guarded by writeMu
	inputErr     error
	state        serverState

	// Coalesce high-frequency streaming deltas only when output is under backpressure.
	deltaPending bool
	deltaEvent   Event
	deltaBuf     strings.Builder
	deltaTimer   *time.Timer

	workMu        sync.Mutex // guards the work queue and its state
	work          []workItem
	workReady     *sync.Cond
	promptRunning bool // from dequeue until the turn settles
	closed        bool
	detached      bool // true when output failed and an active turn is finishing in grace window
	steeringTurn  string
	steeringQueue []string // selected prompt IDs, in selection order
	steeringClaim string   // currently being saved to the active turn

	done     chan struct{}
	doneOnce sync.Once

	approvals       map[string]pendingApproval
	approvalsMu     sync.Mutex
	apprSeq         int
	approvalTimeout time.Duration

	// continuations are the doom-loop guard's pending prompts, by
	// id, like approvals but on a separate channel: the question is
	// not a permission, so its answer is not an approval_response.
	continuations   map[string]chan agent.ContinuationDecision
	continuationsMu sync.Mutex
	contSeq         int

	// questions are the ask_user tool's pending structured
	// questions, by id — a third channel, again separate from
	// approvals: an answer is a selection, not a permission.
	questions   map[string]chan questionAnswer
	questionsMu sync.Mutex
	questSeq    int

	turnMu        sync.Mutex
	turnCtx       context.Context
	turnCancel    context.CancelFunc
	turnAborted   bool
	turnID        string
	turnRequestID string
	stepID        string

	// turnUsage / turnOutcome are agent-goroutine-only: the loop emits
	// events synchronously on the Run goroutine, and runTurn reads them
	// on the same goroutine after Handle returns.
	turnUsage         llm.Usage
	turnOutcome       *turnOutcome
	backgroundOrigins map[string]eventOrigin
}

// Run serves RPC on in/out until the GUI quits or closes stdin. It
// returns nil on an orderly shutdown.
func Run(a *app.App, runner *app.Runner, in io.Reader, out io.Writer) error {
	s := &Server{
		app:           a,
		runner:        runner,
		in:            in,
		out:           out,
		done:          make(chan struct{}),
		approvals:     map[string]pendingApproval{},
		continuations: map[string]chan agent.ContinuationDecision{},
		questions:     map[string]chan questionAnswer{},
	}
	s.workReady = sync.NewCond(&s.workMu)
	return s.run()
}

func (s *Server) run() error {
	defer handleStdoutBrokenPipe(s.out)()
	if err := s.runner.RefreshUsage(); err != nil {
		return err
	}
	s.approvalTimeout = time.Duration(config.DefaultApprovalTimeoutSeconds) * time.Second
	if s.app != nil && s.app.Config != nil && s.app.Config.ApprovalTimeoutSeconds != nil {
		s.approvalTimeout = time.Duration(*s.app.Config.ApprovalTimeoutSeconds) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv("HARNESS_APPROVAL_TIMEOUT_SECONDS")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
			s.approvalTimeout = time.Duration(secs) * time.Second
		}
	}
	s.connectionID = rand.Text()
	s.output = newProtocolOutput(s.out, func() {
		s.detach()
		if c, ok := s.in.(io.Closer); ok {
			_ = c.Close()
		}
	})
	s.app.Cfg.OnEvent = s.onEvent
	s.app.Cfg.Ask = s.ask
	s.app.Cfg.ConfirmContinuation = s.confirmContinuation
	s.app.Cfg.AskQuestion = s.askQuestion
	s.app.Cfg.ReceiveSteering = s.receiveSteering
	// A successful compaction (manual or automatic) is reported on
	// stdout so the GUI learns the history was replaced by a
	// summary; the CLI and TUI leave OnCompact nil and keep their
	// [compact] print.
	s.runner.OnCompact = func(messages, summaryTokens int) {
		s.writeEvent(Event{Type: "compacted", Messages: messages, SummaryTokens: summaryTokens})
	}
	s.refreshState()
	fmt.Fprintf(os.Stderr, "harness rpc ready: session %s (stdin/stdout JSONL)\n", s.app.Store.ID)
	s.writeEvent(Event{
		Type: "ready", Protocol: protocolVersion,
		Session: s.app.Store.ID, Model: s.app.Cfg.Client.Model, API: s.app.Cfg.Client.API,
		Mode: s.app.Cfg.ApprovalMode(), Root: s.app.Root,
		Resumed: s.app.ResumedMessage != "", Messages: len(s.app.History),
		ResumedMessage: s.app.ResumedMessage, RecoveredJobs: s.app.RecoveredJobs,
	})
	go s.readLoop()
	s.agentLoop()
	if c, ok := s.in.(io.Closer); ok {
		_ = c.Close()
	}
	closeErr := s.app.Close()
	s.flushDelta()
	outputErr := s.output.finish()
	s.mu.Lock()
	inputErr := s.inputErr
	s.mu.Unlock()
	return errors.Join(closeErr, outputErr, inputErr)
}

// readLoop decodes stdin lines (LF only) until EOF, then shuts down.
func (s *Server) readLoop() {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if s.isDone() {
			return
		}
		s.handleLine(sc.Text())
	}
	if err := sc.Err(); err != nil && !s.isDone() && !s.isDetached() {
		s.mu.Lock()
		s.inputErr = fmt.Errorf("read rpc request: %w", err)
		s.mu.Unlock()
		s.writeEvent(Event{Type: "error", Error: "rpc request exceeds the size limit or input failed"})
	}
	if s.isDetached() {
		return
	}
	s.shutdown()
}

func (s *Server) handleLine(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	req, err := decodeRequest(line)
	if err != nil {
		s.writeEvent(Event{Type: "error", Error: fmt.Sprintf("invalid request: %v", err)})
		return
	}
	switch req.Type {
	case "approval_response":
		s.handleApprovalResponse(req)
	case "continuation_response":
		s.handleContinuationResponse(req)
	case "question_response":
		s.handleQuestionResponse(req)
	case "abort":
		s.handleAbort(req)
	case "get_state", "usage", "jobs", "list_sessions", "list_plugins", "list_models", "list_skills", "history", "replay":
		s.handleQuery(req)
	case "prompt":
		s.handlePrompt(req)
	case "clear_queue":
		s.handleClearQueue(req)
	case "steer_prompt":
		s.handleSteerPrompt(req)
	case "set_approval_mode":
		s.handleSetApprovalMode(req)
	case "clear", "compact", "new_session", "skill", "quit", "attach_image", "attach_file", "list_pending_files", "set_model", "rename_session", "resume_session", "steal_session", "delete_session", "set_tool_mode", "rollback", "prune_artifacts", "stop_job", "command":
		s.enqueue(req, workControl)
	default:
		s.respond(req, nil, fmt.Errorf("unknown request type %q", req.Type))
	}
}

// handlePrompt enqueues a task. A known slash line ("/usage", "/jobs",
// "/clear", "/compact", "/skill:<name>", "/quit") behaves as the
// matching control request, exactly like the REPL; any other line,
// including an unknown "/foo", is a task for the model. The response
// says whether the turn started at once or queued behind one.
func (s *Server) handlePrompt(req Request) {
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		s.respond(req, nil, errors.New("prompt message is empty"))
		return
	}
	if typ, arg := slashRequest(req.Message); typ != "" {
		ctrl := Request{ID: req.ID, Type: typ, Name: arg}
		if typ == "attach_image" || typ == "attach_file" {
			ctrl = Request{ID: req.ID, Type: typ, Path: arg}
		}
		if typ == "set_model" {
			prov, mdl := app.SplitModelArg(arg)
			ctrl = Request{ID: req.ID, Type: typ, Provider: prov, Model: mdl}
		}
		if typ == "set_approval_mode" {
			ctrl.Mode = arg
		}
		if typ == "resume_session" {
			ctrl = Request{ID: req.ID, Type: typ, Session: arg}
		}
		if typ == "stop_job" {
			if strings.TrimSpace(arg) == "" {
				s.respond(req, nil, fmt.Errorf("stop_job needs a job id"))
				return
			}
			ctrl = Request{ID: req.ID, Type: typ, JobID: arg}
		}
		if typ == "rollback" {
			n := 1
			if arg != "" {
				if v, err := strconv.Atoi(arg); err != nil || v <= 0 {
					s.respond(req, nil, fmt.Errorf("rollback needs a positive turn count"))
					return
				} else {
					n = v
				}
			}
			ctrl = Request{ID: req.ID, Type: typ, Count: n}
		}
		if typ == "rename_session" {
			parts := strings.Fields(arg)
			if len(parts) == 2 {
				ctrl = Request{ID: req.ID, Type: typ, Name: parts[0], New: parts[1]}
			}
		}
		if typ == "usage" || typ == "jobs" || typ == "list_sessions" || typ == "list_plugins" || typ == "get_state" {
			// Read-only: answer at once, like the request of the same name.
			s.handleQuery(ctrl)
			return
		}
		if typ == "set_approval_mode" {
			s.handleSetApprovalMode(ctrl)
			return
		}
		s.enqueue(ctrl, workControl)
		return
	}
	// A plugin command ("/plan", "/execute", ...) behaves as the
	// matching control request, exactly like the REPL; any other
	// line is a task for the model.
	if name, args, ok := app.ParsePluginCommand(req.Message); ok && s.app.Plugins != nil && s.app.Plugins.HasCommand(name) {
		s.enqueue(Request{ID: req.ID, Type: "command", Name: name, Message: args}, workControl)
		return
	}
	req.TurnID = rand.Text()
	accepted := make(chan struct{})
	req.accepted = accepted
	s.enqueue(req, workPrompt)
	close(accepted)
}

// slashRequest maps a REPL slash line to the RPC request type it
// behaves as, or "" when the line is a task for the model.
func slashRequest(line string) (typ, arg string) {
	switch {
	case line == "/quit" || line == "/exit":
		return "quit", ""
	case line == "/prune-artifacts":
		return "prune_artifacts", ""
	case line == "/usage":
		return "usage", ""
	case line == "/jobs":
		return "jobs", ""
	case line == "/clear":
		return "clear", ""
	case line == "/compact":
		return "compact", ""
	case strings.HasPrefix(line, "/skill:"):
		return "skill", strings.TrimPrefix(line, "/skill:")
	case line == "/image" || strings.HasPrefix(line, "/image "):
		return "attach_image", strings.TrimSpace(strings.TrimPrefix(line, "/image"))
	case line == "/attach":
		return "list_pending_files", ""
	case strings.HasPrefix(line, "/attach "):
		return "attach_file", strings.TrimSpace(strings.TrimPrefix(line, "/attach"))
	case line == "/model" || strings.HasPrefix(line, "/model "):
		return "set_model", strings.TrimSpace(strings.TrimPrefix(line, "/model"))
	case line == "/permissions":
		return "get_state", ""
	case strings.HasPrefix(line, "/permissions "):
		return "set_approval_mode", strings.TrimSpace(strings.TrimPrefix(line, "/permissions"))
	case line == "/sessions":
		return "list_sessions", ""
	case line == "/plugins":
		return "list_plugins", ""
	case line == "/resume" || strings.HasPrefix(line, "/resume "):
		return "resume_session", strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
	case line == "/rollback" || strings.HasPrefix(line, "/rollback "):
		return "rollback", strings.TrimSpace(strings.TrimPrefix(line, "/rollback"))
	case line == "/rename" || strings.HasPrefix(line, "/rename "):
		return "rename_session", strings.TrimSpace(strings.TrimPrefix(line, "/rename"))
	default:
		if arg, matched := app.ParseJobsStopArg(line); matched {
			return "stop_job", arg
		}
		return "", ""
	}
}

// enqueue acknowledges prompts immediately. Control requests receive their
// response from the agent goroutine, after the control has run.
func (s *Server) enqueue(req Request, kind workKind) {
	s.workMu.Lock()
	if s.closed {
		s.workMu.Unlock()
		s.respond(req, nil, errShutdown)
		return
	}
	if len(s.work) == maxQueue {
		s.workMu.Unlock()
		s.respond(req, nil, errors.New("prompt queue is full"))
		return
	}
	disposition := "started"
	if s.promptRunning || len(s.work) > 0 {
		disposition = "queued"
	}
	s.work = append(s.work, workItem{kind: kind, req: req})
	s.workReady.Signal()
	s.workMu.Unlock()
	if kind == workPrompt {
		s.respond(req, map[string]any{"disposition": disposition, "turn_id": req.TurnID}, nil)
	}
}

func (s *Server) handleApprovalResponse(req Request) {
	s.approvalsMu.Lock()
	pending, ok := s.approvals[req.ApprovalID]
	if ok {
		delete(s.approvals, req.ApprovalID)
	}
	s.approvalsMu.Unlock()
	if !ok {
		s.respond(req, nil, fmt.Errorf("unknown approval id %q", req.ApprovalID))
		return
	}
	select {
	case pending.ch <- approvalAnswer{approved: req.Approved}:
	default:
	}
	s.respond(req, map[string]any{"ok": true}, nil)
}

// handleSetApprovalMode runs on the reader goroutine so mode switches take
// effect immediately without waiting behind a running turn or blocked dialog.
func (s *Server) handleSetApprovalMode(req Request) {
	s.approvalsMu.Lock()
	if err := s.app.SetApprovalMode(req.Mode); err != nil {
		s.approvalsMu.Unlock()
		s.respond(req, nil, err)
		return
	}
	s.mu.Lock()
	s.state.mode = req.Mode
	s.mu.Unlock()
	if req.Mode == approvalmode.FullAccess || req.Mode == approvalmode.Auto {
		for id, pending := range s.approvals {
			if !s.app.Cfg.ApprovalRequired(req.Mode, pending.req) {
				delete(s.approvals, id)
				select {
				case pending.ch <- approvalAnswer{approved: true}:
				default:
				}
			}
		}
	}
	s.approvalsMu.Unlock()
	s.respond(req, map[string]any{"mode": req.Mode}, nil)
}

// handleAbort cancels the running turn (if any) and fails approvals
// waiting on a dialog: the loop blocks in ask(), which no context
// cancellation can reach, so the approval must be failed directly.
func (s *Server) handleAbort(req Request) {
	s.turnMu.Lock()
	cancel := s.turnCancel
	if cancel != nil {
		s.turnAborted = true
		s.closeSteering()
		cancel()
	}
	s.failApprovals(errAborted)
	s.failContinuations(errAborted)
	s.failQuestions(errAborted)
	// Keep the next queued runTurn from registering new waiters until every
	// waiter owned by the cancelled turn has been detached.
	s.turnMu.Unlock()
	s.respond(req, map[string]any{"aborted": cancel != nil}, nil)
}

// handleClearQueue drops queued prompts at once, from the reader
// goroutine. It must run here, not as a work item: a queued control
// item would run first if clear_queue itself waited in the FIFO,
// and by then the queued prompt it was meant to drop has already
// started. The running turn is unaffected (send
// clear_queue first, then abort).
func (s *Server) handleClearQueue(req Request) {
	s.workMu.Lock()
	before := len(s.work)
	// Steering already in progress has its own copy. Remove its queued copy
	// too so a failed delivery can't start another turn after clear_queue.
	s.work = slices.DeleteFunc(s.work, func(item workItem) bool { return item.kind == workPrompt })
	s.steeringQueue = nil
	cleared := before - len(s.work)
	s.workMu.Unlock()
	s.respond(req, map[string]any{"cleared": cleared}, nil)
}

func (s *Server) failApprovals(err error) {
	s.approvalsMu.Lock()
	pending := s.approvals
	s.approvals = map[string]pendingApproval{}
	s.approvalsMu.Unlock()
	for _, item := range pending {
		select {
		case item.ch <- approvalAnswer{err: err}:
		default:
		}
	}
}

func (s *Server) handleQuery(req Request) {
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	switch req.Type {
	case "get_state":
		s.workMu.Lock()
		queued := len(s.work)
		s.workMu.Unlock()
		data := map[string]any{
			"session": st.sessionID, "model": st.model, "api": st.api,
			"mode": st.mode, "tool_mode": st.toolMode, "root": st.root, "streaming": st.streaming,
			"workspace_trusted": st.workspaceTrusted,
			"queued":            queued, "messages": st.messages, "turns": st.turns,
			"metering_available":         st.meteringAvailable,
			"session_usage_complete":     st.sessionUsageComplete,
			"current_run_usage_complete": st.runUsageComplete,
			// The effective window and output cap for the current
			// model (Setup/SwitchModel resolved flag/env/model/
			// config/default), so the GUI can meter context against
			// the limit the compact trigger uses.
			"context_window": st.contextWindow, "max_tokens": st.maxTokens,
		}
		if st.meteringAvailable {
			data["total_tokens"] = st.sessionUsage.TotalTokens
		}
		s.respond(req, data, nil)
	case "usage":
		s.mu.Lock()
		st := s.state
		s.mu.Unlock()
		data := map[string]any{
			"metering_available":         st.meteringAvailable,
			"session_usage_complete":     st.sessionUsageComplete,
			"current_run_usage_complete": st.runUsageComplete,
			"current_run":                usageJSON(st.runUsage),
			"fee":                        s.feeLabel(), "turns": st.turns,
		}
		if st.meteringAvailable {
			data["session"] = usageJSON(st.sessionUsage)
			data["metering"] = st.metering
		}
		if st.runCostKnown {
			data["current_run_cost_usd"] = st.runCost
		}
		if st.meteringAvailable && st.metering.UnpricedAttempts() == 0 && !st.metering.LegacyHistory && !st.metering.RecoveryGap && (st.metering.Attempts > 0 || st.ratesConfigured) {
			c := st.sessionCost
			data["cost_usd"] = &c
		}
		s.respond(req, data, nil)
	case "jobs":
		s.respond(req, map[string]any{"jobs": s.jobsList()}, nil)
	case "list_sessions":
		list, err := session.List()
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		rows := make([]map[string]any, 0, len(list))
		for _, info := range list {
			rows = append(rows, map[string]any{
				"id": info.ID, "messages": info.Messages,
				"modified": info.Modified.Format(time.RFC3339), "preview": info.Preview,
				"current": info.ID == st.sessionID,
			})
		}
		s.respond(req, map[string]any{"sessions": rows}, nil)
	case "list_plugins":
		s.respond(req, map[string]any{"plugins": s.pluginsList()}, nil)
	case "list_models":
		s.respond(req, map[string]any{"providers": s.modelsList()}, nil)
	case "list_skills":
		s.respond(req, map[string]any{"skills": s.skillsList()}, nil)
	case "history":
		id := req.Session
		if id == "" {
			id = st.sessionID
		}
		if err := session.ValidateSessionID(id); err != nil {
			s.respond(req, nil, err)
			return
		}
		msgs, err := session.Load(id)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		if msgs == nil {
			msgs = []llm.Message{}
		}
		s.respond(req, map[string]any{"session": id, "messages": msgs}, nil)
	case "replay":
		s.handleReplay(req)
	}
}

type replayEvent struct {
	sequence uint64
	data     json.RawMessage
}

type replayPage struct {
	ConnectionID      string            `json:"connection_id"`
	LatestSequence    uint64            `json:"latest_sequence"`
	OldestSequence    uint64            `json:"oldest_sequence"`
	NextAfterSequence uint64            `json:"next_after_sequence"`
	Truncated         bool              `json:"truncated"`
	HasMore           bool              `json:"has_more"`
	Events            []json.RawMessage `json:"events"`
	PendingApprovals  []string          `json:"pending_approvals"`
}

func (s *Server) handleReplay(req Request) {
	s.approvalsMu.Lock()
	pendingApprovals := make([]string, 0, len(s.approvals))
	for id := range s.approvals {
		pendingApprovals = append(pendingApprovals, id)
	}
	s.approvalsMu.Unlock()
	slices.Sort(pendingApprovals)

	s.writeMu.Lock()
	s.flushDeltaLocked()
	page := replayPage{
		ConnectionID: s.connectionID, LatestSequence: s.sequence,
		NextAfterSequence: math.MaxUint64, Truncated: req.AfterSequence < s.evictedSeq,
		Events: []json.RawMessage{}, PendingApprovals: pendingApprovals,
	}
	if len(s.eventHistory) > 0 {
		page.OldestSequence = s.eventHistory[0].sequence
	}
	// Reserve the longest sequence numbers and boolean values before adding
	// already encoded events and their separators to the frame.
	envelope, err := encodeLine(Response{ID: req.ID, Type: "response", Sequence: math.MaxUint64, Data: page})
	remaining := maxOutputFrameBytes - len(envelope)
	page.NextAfterSequence = req.AfterSequence
	for _, ev := range s.eventHistory {
		if ev.sequence <= req.AfterSequence {
			continue
		}
		if len(ev.data)+1 > remaining {
			page.HasMore = true
			break
		}
		page.Events = append(page.Events, ev.data)
		page.NextAfterSequence = ev.sequence
		remaining -= len(ev.data) + 1
	}
	s.writeMu.Unlock()
	if err != nil || len(envelope) > maxOutputFrameBytes || (page.HasMore && len(page.Events) == 0) {
		s.respond(req, nil, fmt.Errorf("replay page cannot fit the output frame limit"))
		return
	}
	if !page.HasMore {
		page.NextAfterSequence = max(req.AfterSequence, page.LatestSequence)
	}
	s.writeJSON(Response{ID: req.ID, Type: "response", Data: page})
}

// pluginsList renders the registry's discovery outcome for
// list_plugins: the same information /plugins shows — name, version,
// description, tools, state (loaded / disabled / skipped), and the
// reason as "error" for a skipped plugin.
func (s *Server) pluginsList() []map[string]any {
	if s.app.Plugins == nil {
		return []map[string]any{}
	}
	infos := s.app.Plugins.Infos()
	list := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		item := map[string]any{
			"name": info.Name, "version": info.Version,
			"description": info.Description, "tools": info.Tools,
			"state": info.State,
		}
		if info.Reason != "" {
			item["error"] = info.Reason
		}
		list = append(list, item)
	}
	return list
}

// modelsList renders the resolved providers and their models for
// list_models: the same values App.Setup resolves for the current
// model (per-model overrides applied, defaults filled), not the raw
// config file. Providers are in config order (sorted by name); a
// provider's flat model is listed too when it is not in its models
// array.
func (s *Server) modelsList() []map[string]any {
	cfg := s.app.Config
	if cfg == nil {
		return []map[string]any{}
	}
	list := make([]map[string]any, 0, len(cfg.Providers))
	for _, name := range cfg.ProviderNames() {
		prov := cfg.Providers[name]
		api, _ := config.NormalizeAPI(prov.API)
		item := map[string]any{"name": name, "api": api}
		models := make([]map[string]any, 0, len(prov.Models)+1)
		seen := map[string]bool{}
		add := func(id string) {
			if seen[id] {
				return
			}
			mdl, ok := prov.ResolveModel(id)
			if !ok {
				return
			}
			seen[id] = true
			cw := config.DefaultContextWindow
			if mdl.ContextWindow != nil {
				cw = *mdl.ContextWindow
			}
			mt := config.DefaultMaxTokens
			if mdl.MaxTokens != nil {
				mt = *mdl.MaxTokens
			}
			models = append(models, map[string]any{
				"id": mdl.ID, "name": mdl.Name, "api": mdl.API,
				"context_window": cw, "max_tokens": mt, "input": mdl.Input,
			})
		}
		for _, e := range prov.Models {
			add(e.ID)
		}
		if prov.Model != "" {
			add(prov.Model)
		}
		item["models"] = models
		list = append(list, item)
	}
	return list
}

// skillsList renders the discovered skills for list_skills: the same
// name+description listing the system prompt carries.
func (s *Server) skillsList() []map[string]any {
	list := make([]map[string]any, 0, len(s.app.Skills))
	for _, sk := range s.app.Skills {
		list = append(list, map[string]any{"name": sk.Name, "description": sk.Description})
	}
	return list
}

func (s *Server) jobsList() []map[string]any {
	jobs := s.app.Cfg.Exec.Jobs()
	list := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		snap := j.Snapshot()
		item := map[string]any{
			"id": snap.ID, "status": fmt.Sprintf("%s", snap.Status),
			"elapsed": j.Elapsed().Seconds(), "command": snap.Command,
		}
		if snap.Cwd != "" {
			item["cwd"] = snap.Cwd
		}
		if snap.OutputFile != "" {
			item["output_file"] = snap.OutputFile
		}
		if snap.Identity.PID > 0 {
			item["pid"] = snap.Identity.PID
		}
		if snap.Identity.PGID > 0 {
			item["pgid"] = snap.Identity.PGID
		}
		if snap.Adopted {
			item["adopted"] = true
		}
		if snap.RecoveryNote != "" {
			item["recovery_note"] = snap.RecoveryNote
		}
		if snap.Status == tools.JobDone {
			if !snap.Result.ExitCodeUnknown {
				code := snap.Result.ExitCode
				item["exit_code"] = &code
			}
			if snap.Result.Err != nil {
				item["error"] = snap.Result.Err.Error()
			}
		} else if snap.Status == tools.JobUnknown && snap.RecoveryNote != "" {
			item["error"] = snap.RecoveryNote
		}
		list = append(list, item)
	}
	return list
}

// ask is Config.Ask for RPC mode: it emits the approval_request (with
// the id the GUI must echo) and blocks until the GUI answers, the turn
// is aborted, or the server shuts down. The loop emits EventApproval
// before calling ask; onEvent deliberately ignores that event, or the
// request would go out twice.
func (s *Server) ask(req agent.ApprovalRequest) (bool, error) {
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s.approvalsMu.Lock()
	mode := s.app.Cfg.ApprovalMode()
	if (mode == approvalmode.FullAccess || mode == approvalmode.Auto) && !s.app.Cfg.ApprovalRequired(mode, req) {
		s.approvalsMu.Unlock()
		err := ctx.Err()
		return err == nil, err
	}
	s.apprSeq++
	id := fmt.Sprintf("appr-%d", s.apprSeq)
	ch := make(chan approvalAnswer, 1)
	s.approvals[id] = pendingApproval{ch: ch, req: req}
	s.approvalsMu.Unlock()
	defer func() { s.approvalsMu.Lock(); delete(s.approvals, id); s.approvalsMu.Unlock() }()
	s.writeEvent(Event{Type: "approval_request", ID: id, Tool: req.ToolName, ToolCallID: req.ToolCallID, InvocationID: req.InvocationID, TargetPath: req.TargetPath, Args: parseToolArgs(req.Arguments), Prompt: req.Prompt()})
	var timeoutCh <-chan time.Time
	if s.approvalTimeout > 0 {
		timer := time.NewTimer(s.approvalTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	select {
	case ans := <-ch:
		return ans.approved, ans.err
	case <-timeoutCh:
		return false, fmt.Errorf("%w: approval request %s after %s", agent.ErrApprovalTimedOut, id, s.approvalTimeout)
	case <-ctx.Done():
		return false, ctx.Err()
	case <-s.done:
		return false, errShutdown
	}
}

// confirmContinuation is Config.ConfirmContinuation for RPC mode: it
// emits a continuation_request (with the id the GUI must echo) and
// blocks until the GUI answers, the turn is aborted, or the server
// shuts down. It is a separate channel from approval_request on
// purpose: the doom-loop question is not a permission.
func (s *Server) confirmContinuation(req agent.ContinuationRequest) (agent.ContinuationDecision, error) {
	s.turnMu.Lock()
	ctx := s.turnCtx
	s.turnMu.Unlock()
	if req.Context != nil {
		ctx = req.Context
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, err
	}
	s.continuationsMu.Lock()
	s.contSeq++
	id := fmt.Sprintf("cont-%d", s.contSeq)
	ch := make(chan agent.ContinuationDecision, 1)
	s.continuations[id] = ch
	s.continuationsMu.Unlock()
	defer func() {
		s.continuationsMu.Lock()
		delete(s.continuations, id)
		s.continuationsMu.Unlock()
	}()
	s.writeEvent(Event{
		Type: "continuation_request", ID: id, Tool: req.ToolName,
		Prompt: req.InputPreview, RepeatCount: req.RepeatCount, Threshold: req.Threshold,
	})
	var timeoutCh <-chan time.Time
	if s.approvalTimeout > 0 {
		timer := time.NewTimer(s.approvalTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	select {
	case dec := <-ch:
		return dec, nil
	case <-timeoutCh:
		return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, fmt.Errorf("%w: continuation request %s after %s", agent.ErrApprovalTimedOut, id, s.approvalTimeout)
	case <-ctx.Done():
		return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, ctx.Err()
	case <-s.done:
		return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, errShutdown
	}
}

func (s *Server) handleContinuationResponse(req Request) {
	dec := agent.ContinuationDecision{Outcome: agent.ContinuationOutcome(req.Decision), UpdatedArgs: req.UpdatedArgs}
	switch dec.Outcome {
	case agent.ContinuationOnce, agent.ContinuationSession, agent.ContinuationDeny:
	default:
		s.respond(req, nil, fmt.Errorf("unknown continuation decision %q", req.Decision))
		return
	}
	if dec.UpdatedArgs != "" {
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(dec.UpdatedArgs), &args); err != nil || args == nil {
			s.respond(req, nil, fmt.Errorf("updated_args must be a JSON object"))
			return
		}
	}
	s.continuationsMu.Lock()
	ch, ok := s.continuations[req.ContinuationID]
	if ok {
		delete(s.continuations, req.ContinuationID)
	}
	s.continuationsMu.Unlock()
	if !ok {
		s.respond(req, nil, fmt.Errorf("unknown continuation id %q", req.ContinuationID))
		return
	}
	select {
	case ch <- dec:
	default:
	}
	s.respond(req, map[string]any{"ok": true}, nil)
}

// askQuestion is Config.AskQuestion for RPC mode: it emits a
// question_request and blocks until the GUI answers with
// question_response, the turn is aborted, or the server shuts down.
func (s *Server) askQuestion(req agent.QuestionRequest) (agent.QuestionAnswers, error) {
	s.questionsMu.Lock()
	s.questSeq++
	id := fmt.Sprintf("quest-%d", s.questSeq)
	ch := make(chan questionAnswer, 1)
	s.questions[id] = ch
	s.questionsMu.Unlock()
	defer func() { s.questionsMu.Lock(); delete(s.questions, id); s.questionsMu.Unlock() }()
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s.writeEvent(Event{Type: "question_request", ID: id, Questions: req.Questions})
	var timeoutCh <-chan time.Time
	if s.approvalTimeout > 0 {
		timer := time.NewTimer(s.approvalTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	select {
	case qa := <-ch:
		return qa.ans, qa.err
	case <-timeoutCh:
		return agent.QuestionAnswers{}, fmt.Errorf("%w: question request %s after %s", agent.ErrApprovalTimedOut, id, s.approvalTimeout)
	case <-ctx.Done():
		return agent.QuestionAnswers{}, ctx.Err()
	case <-s.done:
		return agent.QuestionAnswers{}, errShutdown
	}
}

func (s *Server) handleQuestionResponse(req Request) {
	s.questionsMu.Lock()
	ch, ok := s.questions[req.QuestionID]
	if ok {
		delete(s.questions, req.QuestionID)
	}
	s.questionsMu.Unlock()
	if !ok {
		s.respond(req, nil, fmt.Errorf("unknown question id %q", req.QuestionID))
		return
	}
	select {
	case ch <- questionAnswer{ans: agent.QuestionAnswers{Answers: req.Answers}}:
	default:
	}
	s.respond(req, map[string]any{"ok": true}, nil)
}

func (s *Server) failQuestions(err error) {
	s.questionsMu.Lock()
	pending := s.questions
	s.questions = map[string]chan questionAnswer{}
	s.questionsMu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- questionAnswer{err: err}:
		default:
		}
	}
}

func (s *Server) failContinuations(err error) {
	s.continuationsMu.Lock()
	pending := s.continuations
	s.continuations = map[string]chan agent.ContinuationDecision{}
	s.continuationsMu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- agent.ContinuationDecision{Outcome: agent.ContinuationDeny}:
		default:
		}
	}
}

// onEvent translates the loop's event stream into RPC events. It runs
// on the agent goroutine (emit is synchronous in Run), so turnUsage
// and turnOutcome need no locking.
func (s *Server) onEvent(ev agent.Event) {
	s.turnMu.Lock()
	s.stepID = ev.StepID
	s.turnMu.Unlock()

	switch ev.Kind {
	case agent.EventTextDelta:
		s.writeEvent(Event{Type: "text_delta", Text: ev.Text})
	case agent.EventThinkingDelta:
		s.writeEvent(Event{Type: "thinking_delta", Text: ev.Text})
	case agent.EventThinkingEnd:
		s.writeEvent(Event{Type: "thinking_end", Text: ev.Text})
	case agent.EventStepUsage:
		if s.turnUsage.CanAdd(ev.Usage) {
			s.turnUsage.Add(ev.Usage)
		}
		s.writeEvent(Event{Type: "step_usage", Usage: usageJSON(ev.Usage)})
	case agent.EventLLMRequest:
		s.writeEvent(Event{Type: "llm_request", Request: ev.Request, Operation: ev.Operation, Attempt: ev.Attempt})
	case agent.EventToolStart:
		s.writeEvent(Event{Type: "tool_start", Tool: ev.ToolName, ToolCallID: ev.ToolCallID, InvocationID: ev.InvocationID, Args: parseToolArgs(ev.ToolArgs)})
	case agent.EventToolEnd:
		out, trunc := capOutput(ev.ToolOutput, maxToolOutputChars)
		e := Event{Type: "tool_end", Tool: ev.ToolName, ToolCallID: ev.ToolCallID, InvocationID: ev.InvocationID, Output: out, Truncated: trunc || ev.ToolTruncated, CountLowerBound: ev.ToolCountLowerBound, Status: string(ev.ToolStatus), ExitCode: ev.ToolExitCode, OutputFile: ev.ToolOutputFile}
		if ev.ToolErr != nil {
			e.Error = ev.ToolErr.Error()
		}
		if ev.ToolDetail != "" {
			e.Detail = ev.ToolDetail
		}
		if ev.ToolName == "grep" || ev.ToolName == "glob" {
			e.Count = ev.ToolCount
		}
		s.writeEvent(e)
	case agent.EventToolBackgrounded:
		if s.backgroundOrigins == nil {
			s.backgroundOrigins = map[string]eventOrigin{}
		}
		s.turnMu.Lock()
		origin := eventOrigin{s.turnID, s.turnRequestID, ev.StepID}
		s.turnMu.Unlock()
		s.backgroundOrigins[ev.InvocationID] = origin
		s.writeEvent(Event{Type: "tool_backgrounded", Tool: ev.ToolName, ToolCallID: ev.ToolCallID, InvocationID: ev.InvocationID, Args: parseToolArgs(ev.ToolArgs), Job: ev.JobID})
	case agent.EventBackgroundWait:
		s.writeEvent(Event{Type: "background_wait", Jobs: ev.Text})
	case agent.EventToolBackgroundDone:
		origin := s.backgroundOrigins[ev.InvocationID]
		delete(s.backgroundOrigins, ev.InvocationID)
		out, trunc := capOutput(ev.ToolOutput, maxToolOutputChars)
		e := Event{OriginTurnID: origin.turnID, OriginRequestID: origin.requestID, OriginStepID: origin.stepID, Type: "tool_background_done", Job: ev.JobID, ToolCallID: ev.ToolCallID, InvocationID: ev.InvocationID, Output: out, Truncated: trunc, Status: string(ev.ToolStatus), ExitCode: ev.ToolExitCode, OutputFile: ev.ToolOutputFile}
		if ev.ToolErr != nil {
			e.Error = ev.ToolErr.Error()
		}
		s.writeEvent(e)
	case agent.EventApproval:
		// Ignored: ask() emits approval_request itself, with the id.
	case agent.EventApprovalReview:
		s.writeEvent(Event{Type: "approval_review", Tool: ev.ToolName, ToolCallID: ev.ToolCallID, InvocationID: ev.InvocationID, Decision: ev.Decision, Text: ev.Text})
	case agent.EventDone:
		s.turnOutcome = &turnOutcome{text: ev.Text, usage: ev.Usage, reason: ev.TerminalReason, outcome: ev.TaskOutcome}
	case agent.EventError:
		// The text survives an error-ended turn: on a step cap it
		// is the grace summary, otherwise the model's last message.
		// Without it the GUI gets an empty agent_settled.text
		// whenever a turn ends in error, even though the turn
		// produced an answer.
		s.turnOutcome = &turnOutcome{text: ev.Text, err: ev.Err, reason: ev.TerminalReason, outcome: ev.TaskOutcome}
	case agent.EventStopBlocked:
		s.writeEvent(Event{Type: "stop_blocked", Text: ev.Text})
	case agent.EventStepRetry:
		s.writeEvent(Event{Type: "step_retry", Text: ev.Text})
	case agent.EventStepDiscard:
		s.writeEvent(Event{Type: "step_discard", Text: ev.Text})
	}
}

// agentLoop runs one work item at a time until shutdown.
func (s *Server) agentLoop() {
	for {
		item, ok := s.takeWork()
		if !ok {
			return
		}
		if item.req.accepted != nil {
			// Publish the acceptance frame before events for this turn.
			<-item.req.accepted
		}
		if s.isDone() {
			return
		}
		switch item.kind {
		case workPrompt:
			s.runTurn(item.req)
		case workControl:
			s.doControl(item.req)
		}
		s.refreshState()
	}
}

func (s *Server) takeWork() (workItem, bool) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	for len(s.work) == 0 && !s.closed {
		s.workReady.Wait()
	}
	if s.closed {
		return workItem{}, false
	}
	item := s.work[0]
	s.work = slices.Delete(s.work, 0, 1)
	s.promptRunning = item.kind == workPrompt
	return item, true
}

func (s *Server) finishPrompt() {
	s.workMu.Lock()
	s.promptRunning = false
	s.workMu.Unlock()
}

func (s *Server) refreshState() {
	s.mu.Lock()
	s.state.messages = len(s.app.History)
	s.state.sessionID = s.app.Store.ID
	s.state.turns = s.runner.Turns
	s.state.sessionUsage = s.runner.SessionUsage
	s.state.runUsage, s.state.runCost, s.state.metering = s.runner.RunUsage, s.runner.RunCost, s.runner.SessionMetering
	s.state.runCostKnown = s.runner.CurrentRunCost() != nil
	s.state.meteringAvailable = s.runner.MeteringAvailable
	s.state.sessionUsageComplete = s.runner.SessionUsageComplete()
	s.state.runUsageComplete = s.runner.RunUsageComplete()
	s.state.sessionCost = s.runner.SessionCost
	s.state.toolMode = string(s.runner.App.ToolMode())
	s.state.model, s.state.api = s.app.Cfg.Client.Model, s.app.Cfg.Client.API
	s.state.mode, s.state.root = s.app.Cfg.ApprovalMode(), s.app.Root
	s.state.workspaceTrusted = s.app.WorkspaceTrusted
	s.state.contextWindow, s.state.maxTokens = s.app.ContextLimit, s.app.Cfg.Client.MaxTokens
	s.state.ratesConfigured = s.app.Rates.Configured
	s.state.fee = s.app.Cfg.Client.ProviderName() + " price unverified"
	s.mu.Unlock()
}

func (s *Server) setStreaming(v bool) {
	s.mu.Lock()
	s.state.streaming = v
	s.mu.Unlock()
}

// runTurn runs one task and ends it with exactly one agent_settled
// event, whatever happened: done, error, or aborted. The GUI waits
// for that event, never for the response (which only says accepted).
func (s *Server) runTurn(req Request) {
	s.setStreaming(true)
	s.turnUsage = llm.Usage{}
	s.turnOutcome = nil
	s.turnMu.Lock()
	s.turnAborted = false
	s.turnID = req.TurnID
	if s.turnID == "" {
		s.turnID = rand.Text()
	}
	s.turnRequestID = req.ID
	s.stepID = ""
	ctx, cancel := context.WithCancel(context.Background())
	s.turnCtx = ctx
	s.turnCancel = cancel
	s.workMu.Lock()
	if s.isDone() {
		cancel()
	} else {
		s.steeringTurn = s.turnID
	}
	s.workMu.Unlock()
	s.turnMu.Unlock()
	s.writeEvent(Event{Type: "turn_started", Text: truncateRunes(req.Message, 200)})
	previousCost := s.runner.SessionCost
	previousMetering := s.runner.SessionMetering
	s.runner.Handle(ctx, req.Message)
	s.turnMu.Lock()
	s.closeSteering()
	s.turnCancel = nil
	s.turnCtx = nil
	aborted := s.turnAborted
	s.turnMu.Unlock()
	cancel()

	usage := s.turnUsage
	var text string
	var err error
	reason := agent.TerminalReason("")
	outcome := agent.TaskOutcome("")
	if out := s.turnOutcome; out != nil {
		text = out.text
		reason = out.reason
		outcome = out.outcome
		if out.err == nil {
			usage = out.usage
		}
	}
	if s.runner.LastErr != nil {
		err = s.runner.LastErr
	}
	if aborted {
		// Cancellation is semantic, not an error (ACP/Codex rule):
		// the settled event carries reason "abort" and no error
		// string. The loop's own error (context cancelled) is
		// discarded here; the GUI reads Aborted + Reason.
		err = nil
		reason = agent.TerminalAbort
		outcome = agent.OutcomeAborted
	} else if reason == "" {
		if err != nil {
			reason = agent.TerminalError
		} else {
			reason = agent.TerminalEndTurn
		}
	}
	if outcome == "" {
		outcome = agent.TaskOutcomeForReason(reason)
	}
	s.setStreaming(false)
	s.refreshState()

	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	settled := Event{
		Type: "agent_settled", Text: text, Usage: usageJSON(usage),
		SessionUsage: usageJSON(st.sessionUsage), RunUsage: usageJSON(st.runUsage), Metering: &st.metering, Fee: s.feeLabel(), Aborted: aborted,
		Reason:   string(reason),
		Outcome:  string(outcome),
		ToolMode: st.toolMode,
	}
	if st.meteringAvailable && s.app.Rates.Configured && st.metering.UnpricedAttempts() == previousMetering.UnpricedAttempts() {
		c := st.sessionCost - previousCost
		settled.CostUSD = &c

	}
	if st.meteringAvailable && st.metering.PricedAttempts > 0 && st.metering.UnpricedAttempts() == 0 && !st.metering.LegacyHistory && !st.metering.RecoveryGap {
		sc := st.sessionCost
		settled.SessionCostUSD = &sc
	}
	settled.MeteringAvailable = &st.meteringAvailable
	settled.SessionUsageComplete = &st.sessionUsageComplete
	settled.RunUsageComplete = &st.runUsageComplete
	if !st.meteringAvailable {
		settled.SessionUsage = nil
		settled.Metering = nil
	}
	if st.runCostKnown {
		cost := st.runCost
		settled.RunCostUSD = &cost
	}
	if err != nil {
		settled.Error = err.Error()
	}
	s.finishPrompt()
	s.writeEvent(settled)
	s.turnMu.Lock()
	s.turnID = ""
	s.turnRequestID = ""
	s.stepID = ""
	s.turnMu.Unlock()
}

func textAttachmentMetadata(file llm.TextAttachment) map[string]any {
	return map[string]any{"path": file.Path, "media_type": "text/plain", "bytes": len(file.Content)}
}

// doControl runs one control request on the agent goroutine.
func (s *Server) doControl(req Request) {
	s.turnMu.Lock()
	s.turnID = rand.Text()
	s.turnRequestID = req.ID
	s.stepID = ""
	s.turnAborted = false
	ctx, cancel := context.WithCancel(context.Background())
	s.turnCtx, s.turnCancel = ctx, cancel
	if s.isDone() {
		cancel()
	}
	s.turnMu.Unlock()
	defer func() {
		cancel()
		s.turnMu.Lock()
		s.turnID, s.turnRequestID, s.stepID = "", "", ""
		s.turnCtx, s.turnCancel = nil, nil
		s.turnMu.Unlock()
	}()
	switch req.Type {
	case "prune_artifacts":
		report, err := session.PruneArtifacts(ctx, time.Now())
		s.respond(req, map[string]any{"deleted_blobs": report.DeletedBlobs, "reclaimed_bytes": report.ReclaimedBytes}, err)
	case "clear":
		s.runner.Handle(ctx, "/clear")
		s.refreshState()
		s.respond(req, map[string]any{"ok": true}, nil)
	case "compact":
		s.runner.Handle(ctx, "/compact")
		s.refreshState()
		if s.runner.LastErr != nil {
			s.respond(req, nil, s.runner.LastErr)
			return
		}
		s.respond(req, map[string]any{"ok": true}, nil)
	case "rollback":
		n := req.Count
		if n <= 0 {
			n = 1
		}
		dropped, archive, err := s.runner.Rollback(n)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{"rolled_back": n, "dropped": dropped, "messages": len(s.app.History), "archive": archive}, nil)
	case "new_session":
		id, err := s.runner.ResetSession()
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{"session": id}, nil)
	case "skill":
		body, ok := s.findSkill(req.Name)
		if !ok {
			s.respond(req, nil, fmt.Errorf("unknown skill: %s", req.Name))
			return
		}
		s.runner.Handle(ctx, "/skill:"+req.Name)
		s.respond(req, map[string]any{"loaded": true, "bytes": len(body)}, nil)
	case "attach_image":
		img, err := s.app.AttachImage(req.Path)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.respond(req, map[string]any{
			"attached": true, "path": img.Path, "media_type": img.MediaType,
			"bytes": img.Bytes, "width": img.Width, "height": img.Height,
		}, nil)
	case "attach_file":
		file, err := s.app.AttachFile(req.Path)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		data := textAttachmentMetadata(file)
		data["attached"] = true
		s.respond(req, data, nil)
	case "list_pending_files":
		files := make([]map[string]any, 0, len(s.app.PendingFiles))
		for _, file := range s.app.PendingFiles {
			files = append(files, textAttachmentMetadata(file))
		}
		s.respond(req, map[string]any{"files": files}, nil)
	case "set_model":
		provName, modelID, api, err := s.app.SwitchModel(req.Provider, req.Model)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{"provider": provName, "model": modelID, "api": api}, nil)
	case "rename_session":
		if req.Name == "" || req.New == "" {
			s.respond(req, nil, fmt.Errorf("rename_session needs {\"name\": old, \"new\": new}"))
			return
		}
		err := s.app.RenameSession(req.Name, req.New)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{"renamed": true, "from": req.Name, "to": req.New}, nil)
	case "resume_session":
		if req.Session == "" {
			s.respond(req, nil, fmt.Errorf("resume_session needs {\"session\": id}"))
			return
		}
		n, err := s.runner.ResumeSession(req.Session)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{
			"session": req.Session, "messages": n,
			"resumed_message": s.app.ResumedMessage, "recovered_jobs": s.app.RecoveredJobs,
		}, nil)
	case "steal_session":
		if req.Session == "" {
			s.respond(req, nil, fmt.Errorf("steal_session needs {\"session\": id}"))
			return
		}
		if err := session.StealWriterLock(req.Session, s.app.Root); err != nil {
			s.respond(req, nil, err)
			return
		}
		n, err := s.runner.ResumeSession(req.Session)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{
			"session": req.Session, "messages": n, "stolen": true,
			"resumed_message": s.app.ResumedMessage, "recovered_jobs": s.app.RecoveredJobs,
		}, nil)
	case "delete_session":
		if req.Session == "" {
			s.respond(req, nil, fmt.Errorf("delete_session needs {\"session\": id}"))
			return
		}
		if req.Session == s.app.Store.ID {
			s.respond(req, nil, fmt.Errorf("cannot delete the current session %q (resume or start another session first)", req.Session))
			return
		}
		if err := session.DeleteSession(req.Session); err != nil {
			s.respond(req, nil, err)
			return
		}
		s.respond(req, map[string]any{"deleted": true, "session": req.Session}, nil)
	case "set_tool_mode":
		if err := s.app.SetToolMode(agent.ToolMode(req.Mode)); err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{"tool_mode": string(s.app.ToolMode())}, nil)
	case "quit":
		if err := s.app.Close(); err != nil {
			s.respond(req, nil, err)
			return
		}
		s.respond(req, map[string]any{"ok": true}, nil)
		s.shutdown()
	case "stop_job":
		if req.JobID == "" {
			s.respond(req, nil, fmt.Errorf("stop_job needs {\"job_id\": id}"))
			return
		}
		res, err := s.app.Cfg.Exec.StopJob(req.JobID)
		if s.app.Store != nil {
			if saved, saveErr := agent.SaveJobResultsMessages(s.app.Store, s.app.Cfg.Exec.Jobs()); saveErr == nil && len(saved) > 0 {
				s.app.History = append(s.app.History, saved...)
			}
		}
		s.refreshState()
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.respond(req, map[string]any{
			"job_id": req.JobID, "text": res.Text,
			"exit_code": res.ExitCode, "output_file": res.OutputFile,
		}, nil)
	case "command":
		out, err := s.runner.RunCommand(ctx, req.Name, req.Message)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.refreshState()
		s.respond(req, map[string]any{"ok": true, "output": out}, nil)
	default:
		s.respond(req, nil, fmt.Errorf("unknown control request %q", req.Type))
	}
}

// findSkill returns a skill's full text for the skill request.
func (s *Server) findSkill(name string) (string, bool) {
	for _, sk := range s.app.Skills {
		if sk.Name == name {
			body, err := sk.Load()
			if err != nil {
				return "", false
			}
			return body, true
		}
	}
	return "", false
}

func rpcDetachGrace() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("HARNESS_RPC_DETACH_GRACE_SECONDS")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultDetachGrace
}

func (s *Server) isDetached() bool {
	s.workMu.Lock()
	d := s.detached
	s.workMu.Unlock()
	return d
}

// detach stops accepting new queued turns after an output/client disconnect
// and gives any already-running turn a bounded grace window to finish and
// persist its transcript before cancelling.
func (s *Server) detach() {
	s.workMu.Lock()
	s.detached = true
	if !s.closed {
		s.closed = true
		s.work = nil
		s.steeringTurn = ""
		s.steeringQueue = nil
		s.workReady.Signal()
	}
	s.workMu.Unlock()
	s.failApprovals(agent.ErrApprovalTimedOut)
	s.failContinuations(agent.ErrApprovalTimedOut)
	s.failQuestions(agent.ErrApprovalTimedOut)
	s.turnMu.Lock()
	active := s.turnCancel != nil
	s.turnMu.Unlock()
	grace := rpcDetachGrace()
	if !active || grace <= 0 {
		s.shutdown()
		return
	}
	time.AfterFunc(grace, s.shutdown)
}

func (s *Server) shutdown() {
	s.doneOnce.Do(func() { close(s.done) })
	s.workMu.Lock()
	if !s.closed {
		s.closed = true
		s.work = nil
		s.steeringTurn = ""
		s.steeringQueue = nil
		s.workReady.Signal()
	}
	s.workMu.Unlock()
	s.turnMu.Lock()
	if s.turnCancel != nil {
		s.turnAborted = true
		s.turnCancel()
	}
	s.turnMu.Unlock()
	s.failApprovals(errShutdown)
	s.failContinuations(errShutdown)
	s.failQuestions(errShutdown)
}

func (s *Server) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Server) feeLabel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.fee
}

func usageJSON(u llm.Usage) *UsageJSON {
	return &UsageJSON{Prompt: u.PromptTokens, Cached: u.CachedTokens, Completion: u.CompletionTokens, Total: u.TotalTokens, Reasoning: u.ReasoningTokens, CacheUsageReported: u.CacheUsageReported, CacheUsageSource: u.CacheUsageSource, CacheReportedPromptTokens: u.CacheReportedPromptTokens}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (s *Server) writeEvent(ev Event) {
	if ev.Session == "" {
		s.mu.Lock()
		ev.Session = s.state.sessionID
		s.mu.Unlock()
	}
	if ev.Type != "ready" && ev.Type != "error" {
		s.turnMu.Lock()
		if ev.TurnID == "" {
			ev.TurnID = s.turnID
		}
		if ev.RequestID == "" {
			ev.RequestID = s.turnRequestID
		}
		if ev.StepID == "" {
			ev.StepID = s.stepID
		}
		s.turnMu.Unlock()
	}
	ev.ConnectionID = s.connectionID
	if (ev.Type == "text_delta" || ev.Type == "thinking_delta") && s.output != nil && s.output.pendingFrames() >= maxOutputFrames/2 {
		s.coalesceDelta(ev)
		return
	}
	s.writeJSON(ev)
}

func (s *Server) coalesceDelta(ev Event) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.deltaPending && (s.deltaEvent.Type != ev.Type || s.deltaEvent.TurnID != ev.TurnID || s.deltaEvent.StepID != ev.StepID) {
		s.flushDeltaLocked()
	}
	if !s.deltaPending {
		s.deltaPending = true
		s.deltaEvent = ev
		s.deltaEvent.Text = ""
		s.deltaBuf.Reset()
	}
	s.deltaBuf.WriteString(ev.Text)
	if s.deltaBuf.Len() >= 32<<10 {
		s.flushDeltaLocked()
		return
	}
	if s.deltaTimer == nil {
		s.deltaTimer = time.AfterFunc(25*time.Millisecond, s.flushDelta)
	}
}

func (s *Server) flushDelta() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.flushDeltaLocked()
}

func (s *Server) flushDeltaLocked() {
	if s.deltaTimer != nil {
		s.deltaTimer.Stop()
		s.deltaTimer = nil
	}
	if !s.deltaPending {
		return
	}
	ev := s.deltaEvent
	ev.Text = s.deltaBuf.String()
	s.deltaBuf.Reset()
	s.deltaPending = false
	s.writeFrameLocked(ev)
}

func (s *Server) respond(req Request, data map[string]any, err error) {
	resp := Response{ID: req.ID, Type: "response", Data: data}
	if err != nil {
		resp.Type = "error"
		resp.Error = err.Error()
		resp.Data = nil
	}
	s.writeJSON(resp)
}

// writeJSON serializes frames without holding the read-only state lock.
func (s *Server) writeJSON(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.flushDeltaLocked()
	s.writeFrameLocked(v)
}

func (s *Server) writeFrameLocked(v any) {
	s.sequence++
	switch value := v.(type) {
	case Event:
		value.Sequence = s.sequence
		v = value
	case Response:
		value.Sequence = s.sequence
		v = value
	}
	b, err := encodeLine(v)
	if err != nil {
		if s.output != nil {
			s.output.fail(fmt.Errorf("encode rpc output: %w", err))
		}
		return
	}
	if _, isEvent := v.(Event); isEvent {
		s.retainReplayEvent(b[:len(b)-1])
	}
	if s.output != nil {
		s.output.enqueue(b)
		return
	}
	// Direct, synchronous helper use in package tests. Run always installs the
	// bounded writer before starting either producer goroutine.
	_, _ = s.out.Write(b)
}

// retainReplayEvent records an encoded event while writeMu is held.
func (s *Server) retainReplayEvent(data []byte) {
	if len(data) > maxReplayEventSize {
		// Preserve a contiguous suffix and report the oversized event as a gap.
		s.eventHistory = nil
		s.replayBytes = 0
		s.evictedSeq = s.sequence
		return
	}
	for len(s.eventHistory) >= maxReplayEvents || s.replayBytes+len(data) > maxReplayBytes {
		oldest := s.eventHistory[0]
		s.evictedSeq = oldest.sequence
		s.replayBytes -= len(oldest.data)
		s.eventHistory[0] = replayEvent{}
		s.eventHistory = s.eventHistory[1:]
	}
	s.eventHistory = append(s.eventHistory, replayEvent{sequence: s.sequence, data: data})
	s.replayBytes += len(data)
}
