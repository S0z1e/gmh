// Package rpc's server: one harness process speaking JSONL over stdio.
//
// Concurrency is deliberately boring: a reader goroutine decodes stdin
// and answers read-only queries (get_state, usage, jobs) immediately;
// everything that touches history or the session goes through one work
// queue, and a single agent goroutine runs the items in order — the
// same shape as the TUI's lines channel. Approvals and aborts are the
// only things the reader handles out of band: the agent goroutine
// blocks in Config.Ask, and the reader resolves it when the GUI
// answers; abort cancels the turn's context and fails any pending
// approval, so a turn waiting on a dialog can still be stopped.
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
	"strings"
	"sync"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
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
}

type turnOutcome struct {
	text  string
	usage llm.Usage
	err   error
}

type approvalAnswer struct {
	approved bool
	err      error
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
		app:       a,
		runner:    runner,
		in:        in,
		out:       out,
		work:      make(chan workItem, maxQueue),
		done:      make(chan struct{}),
		approvals: map[string]chan approvalAnswer{},
	}
	return s.run()
}

func (s *Server) run() error {
	s.app.Cfg.OnEvent = s.onEvent
	s.app.Cfg.Ask = s.ask
	s.state.sessionID = s.app.Store.ID
	s.state.messages = len(s.app.History)
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
			// EOF or read error: orderly shutdown, like Pi's Stop.
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
	case "abort":
		s.handleAbort(req)
	case "get_state", "usage", "jobs", "list_sessions", "list_plugins":
		s.handleQuery(req)
	case "prompt":
		s.handlePrompt(req)
	case "clear", "compact", "new_session", "skill", "quit", "clear_queue", "attach_image", "set_model", "rename_session", "resume_session":
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
	s.respond(req, map[string]any{"aborted": cancel != nil}, nil)
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
			"mode": s.app.Cfg.Mode, "root": s.app.Root, "streaming": st.streaming,
			"queued": len(s.work), "messages": st.messages, "turns": st.turns,
			"total_tokens": st.sessionUsage.TotalTokens,
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

// onEvent translates the loop's event stream into RPC events. It runs
// on the agent goroutine (emit is synchronous in Run), so turnUsage
// and turnOutcome need no locking.
func (s *Server) onEvent(ev agent.Event) {
	switch ev.Kind {
	case agent.EventTextDelta:
		s.writeEvent(Event{Type: "text_delta", Text: ev.Text})
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
		s.turnOutcome = &turnOutcome{text: ev.Text, usage: ev.Usage}
	case agent.EventError:
		s.turnOutcome = &turnOutcome{err: ev.Err}
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
	if out := s.turnOutcome; out != nil {
		text = out.text
		if out.err == nil {
			usage = out.usage
		}
	}
	if s.runner.LastErr != nil {
		err = s.runner.LastErr
	}
	if aborted {
		err = errAborted
	}
	s.setStreaming(false)
	s.refreshState()

	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	settled := Event{
		Type: "agent_settled", Text: text, Usage: usageJSON(usage),
		SessionUsage: usageJSON(st.sessionUsage), Fee: s.feeLabel(), Aborted: aborted,
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
	case "quit":
		s.respond(req, map[string]any{"ok": true}, nil)
		s.shutdown()
	case "clear_queue":
		cleared := s.drainPrompts()
		// Drained prompts never settle, so they leave the
		// outstanding count here instead of in runTurn.
		s.addOutstanding(-cleared)
		s.respond(req, map[string]any{"cleared": cleared}, nil)
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
// order. It runs on the agent goroutine; workMu keeps the reader from
// enqueueing mid-drain.
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
	return s.app.Cfg.Client.ProviderName() + " 實價未驗證"
}

func usageJSON(u llm.Usage) *UsageJSON {
	return &UsageJSON{Prompt: u.PromptTokens, Cached: u.CachedTokens, Completion: u.CompletionTokens, Total: u.TotalTokens}
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
