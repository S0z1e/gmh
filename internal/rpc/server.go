// Package rpc's server: one harness process speaking JSONL over stdio.
//
// Concurrency is deliberately boring: a reader goroutine decodes stdin
// and answers read-only queries (get_state, usage, jobs) immediately;
// everything that touches history or the session goes through one work
// queue, and a single agent goroutine runs the items in order — the
// same shape as the TUI's lines channel. Approvals, aborts and
// clear_queue are the only things the reader handles out of band:
// the agent goroutine blocks in Config.Ask, and the reader resolves
// it when the GUI answers; abort cancels the turn's context and
// fails any pending approval, so a turn waiting on a dialog can
// still be stopped; clear_queue drops queued prompts at once, which
// it could not do as a work item (a queued prompt would run first).
//
// stdout carries protocol data only; diagnostics go to stderr.
package rpc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/config"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

const (
	protocolVersion    = 1
	maxQueue           = 64
	maxToolOutputChars = 32 * 1024
)

var (
	errAborted  = errors.New("turn aborted")
	errShutdown = errors.New("rpc server is shutting down")
)

// serverState is the read-only snapshot a GUI polls with get_state. It
// is refreshed by the agent goroutine after every work item, so the
// reader goroutine can answer without touching runner state.
type serverState struct {
	streaming    bool
	turns        int
	sessionUsage llm.Usage
	sessionCost  float64
	messages     int
	sessionID    string
	// toolMode is the loop's tool mode ("default" or "plan").
	toolMode string
}

type turnOutcome struct {
	text   string
	usage  llm.Usage
	err    error
	reason agent.TerminalReason
}

type approvalAnswer struct {
	approved bool
	err      error
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

	mu    sync.Mutex // guards out + state
	state serverState

	// outstanding counts prompts the reader has enqueued but the
	// agent has not settled yet (running or still queued). The reader
	// increments it on a successful prompt enqueue and the agent
	// decrements it when the turn settles, when clear_queue drops the
	// prompt, or when shutdown drops it. disposition() reads it, so a
	// prompt's started/queued answer never depends on the agent
	// goroutine having been scheduled yet (see handlePrompt).
	outstanding int

	work   chan workItem
	workMu sync.Mutex // serialises enqueue vs close(work)
	closed bool

	done     chan struct{}
	doneOnce sync.Once

	approvals   map[string]chan approvalAnswer
	approvalsMu sync.Mutex
	apprSeq     int

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

	turnMu      sync.Mutex
	turnCancel  context.CancelFunc
	turnAborted bool

	// turnUsage / turnOutcome are agent-goroutine-only: the loop emits
	// events synchronously on the Run goroutine, and runTurn reads them
	// on the same goroutine after Handle returns.
	turnUsage   llm.Usage
	turnOutcome *turnOutcome
}

// Run serves RPC on in/out until the GUI quits or closes stdin. It
// returns nil on an orderly shutdown.
func Run(a *app.App, runner *app.Runner, in io.Reader, out io.Writer) error {
	s := &Server{
		app:           a,
		runner:        runner,
		in:            in,
		out:           out,
		work:          make(chan workItem, maxQueue),
		done:          make(chan struct{}),
		approvals:     map[string]chan approvalAnswer{},
		continuations: map[string]chan agent.ContinuationDecision{},
		questions:     map[string]chan questionAnswer{},
	}
	return s.run()
}

func (s *Server) run() error {
	s.app.Cfg.OnEvent = s.onEvent
	s.app.Cfg.Ask = s.ask
	s.app.Cfg.ConfirmContinuation = s.confirmContinuation
	s.app.Cfg.AskQuestion = s.askQuestion
	// A successful compaction (manual or automatic) is reported on
	// stdout so the GUI learns the history was replaced by a
	// summary; the CLI and TUI leave OnCompact nil and keep their
	// [compact] print.
	s.runner.OnCompact = func(messages, summaryTokens int) {
		s.writeEvent(Event{Type: "compacted", Messages: messages, SummaryTokens: summaryTokens})
	}
	s.state.sessionID = s.app.Store.ID
	s.state.messages = len(s.app.History)
	s.state.toolMode = string(s.app.ToolMode())
	fmt.Fprintf(os.Stderr, "harness rpc ready: session %s (stdin/stdout JSONL)\n", s.app.Store.ID)
	s.writeEvent(Event{
		Type: "ready", Protocol: protocolVersion,
		Session: s.app.Store.ID, Model: s.app.Cfg.Client.Model, API: s.app.Cfg.Client.API,
		Mode: s.app.Cfg.Mode, Root: s.app.Root,
		Resumed: s.app.ResumedMessage != "", Messages: len(s.app.History),
	})
	go s.readLoop()
	s.agentLoop()
	return nil
}

// readLoop decodes stdin lines (LF only) until EOF, then shuts down.
func (s *Server) readLoop() {
	br := bufio.NewReader(s.in)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			s.handleLine(line)
		}
		if err != nil {
			// EOF or read error: orderly shutdown.
			s.shutdown()
			return
		}
	}
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
	case "get_state", "usage", "jobs", "list_sessions", "list_plugins", "list_models", "list_skills", "history":
		s.handleQuery(req)
	case "prompt":
		s.handlePrompt(req)
	case "clear_queue":
		s.handleClearQueue(req)
	case "clear", "compact", "new_session", "skill", "quit", "attach_image", "set_model", "rename_session", "resume_session", "delete_session", "set_tool_mode", "rollback", "command":
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
		if typ == "attach_image" {
			ctrl = Request{ID: req.ID, Type: typ, Path: arg}
		}
		if typ == "set_model" {
			prov, mdl := app.SplitModelArg(arg)
			ctrl = Request{ID: req.ID, Type: typ, Provider: prov, Model: mdl}
		}
		if typ == "resume_session" {
			ctrl = Request{ID: req.ID, Type: typ, Session: arg}
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
		if typ == "usage" || typ == "jobs" || typ == "list_sessions" || typ == "list_plugins" {
			// Read-only: answer at once, like the request of the same name.
			s.handleQuery(ctrl)
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
	// Snapshot the disposition before enqueueing, then count this
	// prompt as outstanding on a successful enqueue. The snapshot is
	// exact because outstanding already counts every prompt enqueued
	// ahead of this one: the reader maintains it synchronously, so it
	// never depends on the agent goroutine having been scheduled to
	// pick the previous prompt up (a turn already running is counted
	// even before its turn_started event). Snapshot after the enqueue
	// would be wrong both ways: the agent may not have set streaming
	// yet (this prompt reads "queued") or may already have (it reads
	// "started" for a turn that did start at once).
	disposition := s.disposition()
	if s.enqueue(req, workPrompt) {
		s.addOutstanding(1)
		s.respond(req, map[string]any{"disposition": disposition}, nil)
	}
}

// slashRequest maps a REPL slash line to the RPC request type it
// behaves as, or "" when the line is a task for the model.
func slashRequest(line string) (typ, arg string) {
	switch {
	case line == "/quit" || line == "/exit":
		return "quit", ""
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
	case line == "/model" || strings.HasPrefix(line, "/model "):
		return "set_model", strings.TrimSpace(strings.TrimPrefix(line, "/model"))
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
		return "", ""
	}
}

func (s *Server) disposition() string {
	s.mu.Lock()
	outstanding := s.outstanding
	s.mu.Unlock()
	if outstanding > 0 || len(s.work) > 0 {
		return "queued"
	}
	return "started"
}

// addOutstanding adjusts the count of prompts enqueued but not yet
// settled. Called by the reader (+1 on a successful prompt enqueue)
// and by the agent (-1 when a turn settles or a queued prompt is
// dropped, -cleared when clear_queue drains prompts).
func (s *Server) addOutstanding(delta int) {
	s.mu.Lock()
	s.outstanding += delta
	s.mu.Unlock()
}

// enqueue puts a work item on the agent goroutine's queue. It
// answers the request itself when the item cannot be queued (the
// server is shutting down, or the queue is full), and reports
// whether the item was queued, so the caller knows whether a
// disposition response is still owed.
func (s *Server) enqueue(req Request, kind workKind) bool {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.closed {
		s.respond(req, nil, errShutdown)
		return false
	}
	select {
	case s.work <- workItem{kind: kind, req: req}:
		return true
	default:
		s.respond(req, nil, errors.New("prompt queue is full"))
		return false
	}
}

func (s *Server) handleApprovalResponse(req Request) {
	s.approvalsMu.Lock()
	ch, ok := s.approvals[req.ApprovalID]
	if ok {
		delete(s.approvals, req.ApprovalID)
	}
	s.approvalsMu.Unlock()
	if !ok {
		s.respond(req, nil, fmt.Errorf("unknown approval id %q", req.ApprovalID))
		return
	}
	select {
	case ch <- approvalAnswer{approved: req.Approved}:
	default:
	}
	s.respond(req, map[string]any{"ok": true}, nil)
}

// handleAbort cancels the running turn (if any) and fails approvals
// waiting on a dialog: the loop blocks in ask(), which no context
// cancellation can reach, so the approval must be failed directly.
func (s *Server) handleAbort(req Request) {
	s.turnMu.Lock()
	cancel := s.turnCancel
	if cancel != nil {
		s.turnAborted = true
		cancel()
	}
	s.turnMu.Unlock()
	s.failApprovals(errAborted)
	s.failContinuations(errAborted)
	s.failQuestions(errAborted)
	s.respond(req, map[string]any{"aborted": cancel != nil}, nil)
}

// handleClearQueue drops queued prompts at once, from the reader
// goroutine. It must run here, not as a work item: a queued control
// item would run first if clear_queue itself waited in the FIFO,
// and by then the queued prompt it was meant to drop has already
// started. Drained prompts never settle, so they leave the
// outstanding count here instead of in runTurn. The running turn
// is untouched: only abort cancels it (send
// clear_queue first, then abort).
func (s *Server) handleClearQueue(req Request) {
	cleared := s.drainPrompts()
	s.addOutstanding(-cleared)
	s.respond(req, map[string]any{"cleared": cleared}, nil)
}

func (s *Server) failApprovals(err error) {
	s.approvalsMu.Lock()
	pending := s.approvals
	s.approvals = map[string]chan approvalAnswer{}
	s.approvalsMu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- approvalAnswer{err: err}:
		default:
		}
	}
}

func (s *Server) handleQuery(req Request) {
	switch req.Type {
	case "get_state":
		s.mu.Lock()
		st := s.state
		s.mu.Unlock()
		s.respond(req, map[string]any{
			"session": st.sessionID, "model": s.app.Cfg.Client.Model, "api": s.app.Cfg.Client.API,
			"mode": s.app.Cfg.Mode, "tool_mode": st.toolMode, "root": s.app.Root, "streaming": st.streaming,
			"queued": len(s.work), "messages": st.messages, "turns": st.turns,
			"total_tokens": st.sessionUsage.TotalTokens,
			// The effective window and output cap for the current
			// model (Setup/SwitchModel resolved flag/env/model/
			// config/default), so the GUI can meter context against
			// the limit the compact trigger uses.
			"context_window": s.app.ContextLimit, "max_tokens": s.app.Cfg.Client.MaxTokens,
		}, nil)
	case "usage":
		s.mu.Lock()
		st := s.state
		s.mu.Unlock()
		data := map[string]any{
			"session": usageJSON(st.sessionUsage),
			"fee":     s.feeLabel(), "turns": st.turns,
		}
		if s.app.Rates.Configured {
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
				"current": info.ID == s.app.Store.ID,
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
			id = s.app.Store.ID
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
	}
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
		item := map[string]any{
			"id": j.ID, "status": fmt.Sprintf("%s", j.Status()),
			"elapsed": j.Elapsed().Seconds(), "command": j.Command,
		}
		if res, ok := j.Result(); ok {
			code := res.ExitCode
			item["exit_code"] = &code
			if res.Err != nil {
				item["error"] = res.Err.Error()
			}
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
func (s *Server) ask(prompt string) (bool, error) {
	s.approvalsMu.Lock()
	s.apprSeq++
	id := fmt.Sprintf("appr-%d", s.apprSeq)
	ch := make(chan approvalAnswer, 1)
	s.approvals[id] = ch
	s.approvalsMu.Unlock()
	tool, args := parseApprovalPrompt(prompt)
	s.writeEvent(Event{Type: "approval_request", ID: id, Tool: tool, Args: args, Prompt: prompt})
	select {
	case ans := <-ch:
		return ans.approved, ans.err
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
	s.continuationsMu.Lock()
	s.contSeq++
	id := fmt.Sprintf("cont-%d", s.contSeq)
	ch := make(chan agent.ContinuationDecision, 1)
	s.continuations[id] = ch
	s.continuationsMu.Unlock()
	s.writeEvent(Event{
		Type: "continuation_request", ID: id, Tool: req.ToolName,
		Prompt: req.InputPreview, RepeatCount: req.RepeatCount, Threshold: req.Threshold,
	})
	select {
	case dec := <-ch:
		return dec, nil
	case <-s.done:
		return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, errShutdown
	}
}

func (s *Server) handleContinuationResponse(req Request) {
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
	dec := agent.ContinuationDecision{Outcome: agent.ContinuationOutcome(req.Decision), UpdatedArgs: req.UpdatedArgs}
	switch dec.Outcome {
	case agent.ContinuationOnce, agent.ContinuationSession, agent.ContinuationDeny:
	default:
		s.respond(req, nil, fmt.Errorf("unknown continuation decision %q", req.Decision))
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
	s.writeEvent(Event{Type: "question_request", ID: id, Questions: req.Questions})
	select {
	case qa := <-ch:
		return qa.ans, qa.err
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
	switch ev.Kind {
	case agent.EventTextDelta:
		s.writeEvent(Event{Type: "text_delta", Text: ev.Text})
	case agent.EventThinkingDelta:
		s.writeEvent(Event{Type: "thinking_delta", Text: ev.Text})
	case agent.EventThinkingEnd:
		s.writeEvent(Event{Type: "thinking_end", Text: ev.Text})
	case agent.EventStepUsage:
		s.turnUsage.Add(ev.Usage)
		s.writeEvent(Event{Type: "step_usage", Usage: usageJSON(ev.Usage)})
	case agent.EventToolStart:
		s.writeEvent(Event{Type: "tool_start", Tool: ev.ToolName, Args: parseToolArgs(ev.ToolArgs)})
	case agent.EventToolEnd:
		out, trunc := capOutput(ev.ToolOutput, maxToolOutputChars)
		e := Event{Type: "tool_end", Tool: ev.ToolName, Output: out, Truncated: trunc}
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
		s.writeEvent(Event{Type: "tool_backgrounded", Tool: ev.ToolName, Args: parseToolArgs(ev.ToolArgs), Job: ev.JobID})
	case agent.EventBackgroundWait:
		s.writeEvent(Event{Type: "background_wait", Jobs: ev.Text})
	case agent.EventToolBackgroundDone:
		out, trunc := capOutput(ev.ToolOutput, maxToolOutputChars)
		e := Event{Type: "tool_background_done", Job: ev.JobID, Output: out, Truncated: trunc}
		if ev.ToolErr != nil {
			e.Error = ev.ToolErr.Error()
		}
		s.writeEvent(e)
	case agent.EventApproval:
		// Ignored: ask() emits approval_request itself, with the id.
	case agent.EventDone:
		s.turnOutcome = &turnOutcome{text: ev.Text, usage: ev.Usage, reason: ev.TerminalReason}
	case agent.EventError:
		s.turnOutcome = &turnOutcome{err: ev.Err, reason: ev.TerminalReason}
	case agent.EventStopBlocked:
		s.writeEvent(Event{Type: "stop_blocked", Text: ev.Text})
	}
}

// agentLoop runs work items in order until the queue closes. Queued
// work is dropped once shutdown starts.
func (s *Server) agentLoop() {
	for item := range s.work {
		if s.isDone() {
			// A prompt dropped at shutdown never settles, so it
			// leaves the outstanding count here instead.
			if item.kind == workPrompt {
				s.addOutstanding(-1)
			}
			continue
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

func (s *Server) refreshState() {
	s.mu.Lock()
	s.state.messages = len(s.app.History)
	s.state.sessionID = s.app.Store.ID
	s.state.turns = s.runner.Turns
	s.state.sessionUsage = s.runner.SessionUsage
	s.state.sessionCost = s.runner.SessionCost
	s.state.toolMode = string(s.runner.App.ToolMode())
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
	ctx, cancel := context.WithCancel(context.Background())
	s.turnCancel = cancel
	s.turnMu.Unlock()
	s.writeEvent(Event{Type: "turn_started", Text: truncateRunes(req.Message, 200)})
	s.runner.Handle(ctx, req.Message)
	s.turnMu.Lock()
	s.turnCancel = nil
	aborted := s.turnAborted
	s.turnMu.Unlock()
	cancel()

	usage := s.turnUsage
	var text string
	var err error
	reason := agent.TerminalReason("")
	if out := s.turnOutcome; out != nil {
		text = out.text
		reason = out.reason
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
	} else if reason == "" {
		if err != nil {
			reason = agent.TerminalError
		} else {
			reason = agent.TerminalEndTurn
		}
	}
	s.setStreaming(false)
	s.refreshState()

	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	settled := Event{
		Type: "agent_settled", Text: text, Usage: usageJSON(usage),
		SessionUsage: usageJSON(st.sessionUsage), Fee: s.feeLabel(), Aborted: aborted,
		Reason:   string(reason),
		ToolMode: st.toolMode,
	}
	if s.app.Rates.Configured {
		c := s.app.Rates.Cost(usage, time.Now())
		settled.CostUSD = &c
		sc := st.sessionCost
		settled.SessionCostUSD = &sc
	}
	if err != nil {
		settled.Error = err.Error()
	}
	s.addOutstanding(-1)
	s.writeEvent(settled)
}

// doControl runs one control request on the agent goroutine.
func (s *Server) doControl(req Request) {
	ctx := context.Background()
	switch req.Type {
	case "clear":
		s.runner.Handle(ctx, "/clear")
		s.refreshState()
		s.respond(req, map[string]any{"ok": true}, nil)
	case "compact":
		s.runner.Handle(ctx, "/compact")
		s.refreshState()
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
		var err error
		if req.Name == s.app.Store.ID {
			err = s.app.Store.Rename(req.New)
		} else {
			err = session.RenameSession(req.Name, req.New)
		}
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
		s.respond(req, map[string]any{"session": req.Session, "messages": n}, nil)
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
		s.respond(req, map[string]any{"ok": true}, nil)
		s.shutdown()
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

// drainPrompts drops queued prompt items, keeping control items in
// order. It runs on the reader goroutine (clear_queue is answered
// immediately, not queued); workMu keeps the reader from enqueueing
// mid-drain, and the agent goroutine only receives from the channel,
// so an item is taken by exactly one of them — a prompt the agent
// already picked up counts as running and is never dropped here.
func (s *Server) drainPrompts() int {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	var keep []workItem
	cleared := 0
	for {
		select {
		case item, ok := <-s.work:
			if !ok {
				return cleared
			}
			if item.kind == workPrompt {
				cleared++
			} else {
				keep = append(keep, item)
			}
		default:
			for _, item := range keep {
				s.work <- item
			}
			return cleared
		}
	}
}

func (s *Server) shutdown() {
	s.doneOnce.Do(func() { close(s.done) })
	s.workMu.Lock()
	if !s.closed {
		s.closed = true
		close(s.work)
	}
	s.workMu.Unlock()
	s.turnMu.Lock()
	if s.turnCancel != nil {
		s.turnAborted = true
		s.turnCancel()
	}
	s.turnMu.Unlock()
	s.failApprovals(errShutdown)
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
	return s.app.Cfg.Client.ProviderName() + " price unverified"
}

func usageJSON(u llm.Usage) *UsageJSON {
	return &UsageJSON{Prompt: u.PromptTokens, Cached: u.CachedTokens, Completion: u.CompletionTokens, Total: u.TotalTokens, Reasoning: u.ReasoningTokens}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (s *Server) writeEvent(ev Event) { s.writeJSON(ev) }

func (s *Server) respond(req Request, data map[string]any, err error) {
	resp := Response{ID: req.ID, Type: "response", Data: data}
	if err != nil {
		resp.Type = "error"
		resp.Error = err.Error()
		resp.Data = nil
	}
	s.writeJSON(resp)
}

// writeJSON writes one JSON line to stdout, serialised: events come
// from the agent goroutine and responses from either goroutine, and
// two writers must never interleave mid-line. Write errors (a GUI
// that went away) are swallowed: the server exits on shutdown anyway.
func (s *Server) writeJSON(v any) {
	b, err := encodeLine(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	_, _ = s.out.Write(b)
	s.mu.Unlock()
}
