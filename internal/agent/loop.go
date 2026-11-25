// Package agent runs the single loop: LLM -> tool calls -> observe -> repeat.
// Approval is a commitment/UX boundary, not a sandbox: with bash available,
// denying write/edit can be bypassed. Run on disposable copies or in a container.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"minimalharness/internal/approvalmode"
	"minimalharness/internal/checkpoint"
	"minimalharness/internal/failureguard"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/textutil"
	"minimalharness/internal/toolargs"
	"minimalharness/internal/tools"
	"minimalharness/internal/turnbudget"
	"minimalharness/internal/verification"
)

type Config struct {
	Client         *llm.Client
	Exec           *tools.Executor
	Tools          []llm.ToolDef
	System         string
	modeMu         sync.RWMutex
	Mode           string // see internal/approvalmode
	ContextLimit   int
	CompactHistory func(context.Context, []llm.Message) (string, llm.Usage, error)
	OnCompact      func(history []llm.Message, usage llm.Usage, archive string)
	MaxSteps       int
	Session        *session.Store
	Verification   *verification.Policy
	verifier       *verificationRun
	TurnBudget     *turnbudget.Policy
	UsageRates     llm.Rates
	LastTurnBudget *turnbudget.State
	resources      *turnResources
	// OnEvent receives every loop event; the caller renders them.
	// (There is no OnText: streamed text arrives as TextDelta events.)
	OnEvent func(Event)
	// ReceiveSteering delivers user follow-ups at model or tool boundaries.
	// When finishing is true and nothing is delivered, it closes intake.
	ReceiveSteering func(finishing bool, deliver func(string) bool)
	// TraceRequests emits content-free request diagnostics and audits each attempt.
	TraceRequests bool
	Ask           func(ApprovalRequest) (bool, error) // nil = use stdin prompt
	// ConfirmContinuation is the doom-loop guard's seam: it asks the
	// user whether a tool call repeated Threshold times may continue,
	// with "once" / "session" / "deny" (and optionally updated
	// arguments). It is deliberately separate from Ask: the question
	// is about the shape of the conversation, not whether a tool
	// may act, so no approval rule or mode may answer it. Nil means
	// nobody can be asked (headless): the guard denies.
	ConfirmContinuation func(req ContinuationRequest) (ContinuationDecision, error)
	// DoomLoopThreshold overrides RepeatedToolCallThreshold (0 = default).
	DoomLoopThreshold int
	// MaxLLMAttempts caps LLM calls per step, including retries of
	// transient failures (0 = default 4: the first attempt plus up
	// to 3 retries; 1 = no retry).
	MaxLLMAttempts int
	// DrainTimeout bounds how long drainPending waits for still-
	// running background jobs when a turn would otherwise end
	// (0 = default 30s). After the bound the turn ends with the
	// jobs still running, and their output paths are reported so
	// the model can read partial output; a job already reported
	// still-running in an earlier turn is not waited on again.
	DrainTimeout time.Duration
	// AskQuestion is the structured-question seam for the ask_user
	// tool: it asks the user the request's questions and returns
	// their answers. Nil means nobody can be asked (headless): the
	// loop tells the model to proceed on its best judgement.
	AskQuestion func(req QuestionRequest) (QuestionAnswers, error)
	// DenyInsteadOfPrompt refuses any tool call that needs approval.
	DenyInsteadOfPrompt bool
	// Headless means there is no interactive approval prompt.
	Headless bool

	// LastPromptTokens is the prompt-token count of the most recent LLM
	// call in the last Run: the current context size, since history is
	// resent in full each time. Used for the compaction trigger.
	LastPromptTokens   int
	lastPromptEstimate int
	// LastTerminalReason is why the most recent Run ended, in the
	// structured form the RPC server reports as agent_settled.reason.
	LastTerminalReason TerminalReason
	LastTaskOutcome    TaskOutcome

	// doomLoop is the per-Config guard: its streak and session
	// allowlist outlive one turn, like Qwen's agent-context guard.
	doomLoop          *DoomLoopGuard
	usageAttempts     int64
	stepID            string
	persistErr        error
	persistErrEmitted bool
	progress          *turnProgress
}

// ApprovalMode returns the current approval policy safely across goroutines.
func (c *Config) ApprovalMode() string {
	c.modeMu.RLock()
	m := c.Mode
	c.modeMu.RUnlock()
	return m
}

// SetApprovalMode updates the approval policy safely across goroutines.
func (c *Config) SetApprovalMode(mode string) {
	c.modeMu.Lock()
	c.Mode = mode
	c.modeMu.Unlock()
}

// DoomLoop returns the Config's guard, creating it on first use.
func (c *Config) DoomLoop() *DoomLoopGuard {
	if c.doomLoop == nil {
		c.doomLoop = NewDoomLoopGuard(c.DoomLoopThreshold)
	}
	return c.doomLoop
}

func (c *Config) emit(e Event) {
	e.StepID = c.stepID
	if e.Kind == EventDone || e.Kind == EventError {
		e = c.budgetTerminal(e)
		if c.persistErr != nil {
			// A storage failure takes priority and is published once, even when
			// it occurred while the budget adapter was saving its outcome.
			e.Kind, e.Err, e.TerminalReason, e.TaskOutcome = EventError, c.persistErr, TerminalError, OutcomeIncomplete
			c.LastTerminalReason = TerminalError
			c.persistErrEmitted = true
		}
	}
	if e.Kind == EventDone || e.Kind == EventError {
		if e.TaskOutcome == "" {
			e.TaskOutcome = TaskOutcomeForReason(e.TerminalReason)
		}
		c.LastTaskOutcome = e.TaskOutcome
	}
	if e.Kind == EventToolEnd || e.Kind == EventToolBackgroundDone {
		if e.ToolErr != nil && e.ToolStatus != tools.ResultDeferred {
			e.ToolStatus = tools.ResultError
		} else if e.ToolStatus == "" {
			e.ToolStatus = tools.ResultSuccess
		}
	}
	if c.OnEvent != nil {
		c.OnEvent(e)
	}
}

func ToolDefs() []llm.ToolDef { return tools.Defs() }

func (c *Config) validateArgs(name, args string) error {
	for _, def := range c.Tools {
		if def.Function.Name == name {
			return toolargs.Validate(def.Function.Parameters, args)
		}
	}
	return fmt.Errorf("tool %s not offered in this turn", name)
}

func (c *Config) invalidArgs(msgs []llm.Message, tc llm.ToolCall, err error) []llm.Message {
	obs := fmt.Sprintf("Error: invalid %s arguments: %v", tc.Function.Name, err)
	c.emit(Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments, ToolCallID: tc.ID})
	c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, ToolOutput: obs, ToolErr: err})
	m := llm.Message{Role: "tool", Kind: notExecutedKind, ToolCallID: tc.ID, ToolError: true, Content: obs}
	if c.Session != nil {
		c.persist(m)
	}
	return append(msgs, m)
}

// checkRepeatedCall runs the doom-loop guard for one requested
// call. It returns the arguments to execute (possibly the user's
// edited version) and whether the call may run. A denied call must
// end the turn: the caller reports it with
// TerminalRepeatedToolCallDenied.
func (c *Config) checkRepeatedCall(ctx context.Context, toolName, argsJSON string) (string, bool, error) {
	guard := c.DoomLoop()
	detected, det := guard.Check(toolName, argsJSON)
	if !detected {
		return argsJSON, true, nil
	}
	// Headless (no one to ask, or approvals denied without asking):
	// deny. A headless run must never assume the user would continue.
	if c.ConfirmContinuation == nil || c.DenyInsteadOfPrompt || ctx.Err() != nil {
		return argsJSON, false, ctx.Err()
	}
	dec, err := c.ConfirmContinuation(ContinuationRequest{
		Context:  ctx,
		ToolName: det.ToolName, RepeatCount: det.RepeatCount,
		Threshold: det.Threshold, InputPreview: det.InputPreview,
	})
	if err != nil {
		return argsJSON, false, err
	}
	key := det.InputKey
	useArgs := argsJSON
	if dec.UpdatedArgs != "" {
		useArgs = c.verificationArguments(toolName, dec.UpdatedArgs)
		key = CanonicalToolKey(toolName, useArgs)
	}
	switch dec.Outcome {
	case ContinuationSession:
		guard.AllowSession(key)
		return useArgs, true, nil
	case ContinuationOnce:
		guard.AllowOnce(key)
		return useArgs, true, nil
	default:
		return argsJSON, false, nil
	}
}

// askUser runs one ask_user tool call: validate the questions,
// ask them through the AskQuestion seam, and render the answers
// as the observation. Invalid arguments and a cancelled question
// are observations, not turn errors, so the model can recover.
func (c *Config) askUser(ctx context.Context, argsJSON string) (string, *string, error) {
	req, err := ParseAskUserArgs(argsJSON)
	if err != nil {
		return "", nil, &tools.InputError{Err: err}
	}
	if c.AskQuestion == nil || ctx.Err() != nil {
		return "No one is available to answer questions right now. Proceed with your best judgement and state the assumptions you made.", nil, nil
	}
	req.Context = ctx
	ans, err := c.AskQuestion(req)
	if errors.Is(err, ErrApprovalDetached) || ctx.Err() != nil {
		if err == nil {
			err = ctx.Err()
		}
		return "", nil, err
	}
	if err != nil {
		return "The questions were cancelled. Proceed with your best judgement and state the assumptions you made.", nil, nil
	}
	for _, a := range ans.Answers {
		if a.Question >= 0 && a.Question < len(req.Questions) && (len(a.Selected) > 0 || strings.TrimSpace(a.Custom) != "") {
			text := FormatAnswers(req, ans)
			data, err := json.Marshal(struct {
				Questions []Question       `json:"questions"`
				Answers   []QuestionAnswer `json:"answers"`
			}{req.Questions, ans.Answers})
			if err != nil {
				return "", nil, err
			}
			source := string(data)
			return text, &source, nil
		}
	}
	return "No answers were provided. Proceed with your best judgement and state the assumptions you made.", nil, nil
}

// ApprovalRequired applies the tool policy to an invocation under the given mode.
func (c *Config) ApprovalRequired(mode string, req ApprovalRequest) bool {
	if mode == approvalmode.FullAccess {
		return false
	}
	if req.Protected {
		return true
	}
	name := req.ToolName
	if name == tools.GitStatusTool && c.Exec != nil && c.Exec.Sandbox {
		return true // linked metadata may be outside the shell sandbox
	}
	if name == "write" || name == "edit" {
		var target struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(req.Arguments), &target) == nil && c.Exec != nil && tools.IsProtectedWorkspacePath(c.Exec.Root, target.Path) {
			return true
		}
	}
	if mode == approvalmode.Auto {
		return false
	}
	// Network and plugin tools can send data or cause side effects.
	switch name {
	case "read", "grep", "glob", "load_skill", "ask_user", tools.GuidanceTool:
		return false
	}
	return true
}

// toolOffered reports whether name is in the turn's offered tool
// list (c.Tools). Plan mode removes write/edit/bash from that list,
// and this is what stops a hallucinated call from running.
func toolOffered(tools []llm.ToolDef, name string) bool {
	for _, d := range tools {
		if d.Function.Name == name {
			return true
		}
	}
	return false
}

// execBash runs a bash tool call, backgrounding it when it runs past
// the executor's threshold. It returns the observation for the model
// and, when backgrounded, the job so Run can feed its result back
// later. A call that finishes within the threshold (or with
// backgrounding disabled) returns a nil job and a normal observation.
// A call with background:true backgrounds at once, without waiting
// out the threshold, for commands that never exit on their own.
func (c *Config) execBash(ctx context.Context, tc llm.ToolCall) (tools.ToolResult, *tools.Job, error) {
	var args tools.BashArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return tools.ToolResult{}, nil, &tools.InputError{Err: fmt.Errorf("bad bash args: %w", err)}
	}
	backgrounded := func(job *tools.Job) (tools.ToolResult, *tools.Job, error) {
		c.emit(Event{Kind: EventToolBackgrounded, ToolName: "bash", ToolCallID: tc.ID, InvocationID: job.InvocationID, ToolArgs: tc.Function.Arguments, JobID: job.ID})
		text := fmt.Sprintf("Command is still running in the background (job %s). Continue with other work; its output will be delivered automatically when it finishes. Do not re-run the command.", job.ID)
		if job.OutputFile != "" {
			text += fmt.Sprintf(" Read partial output with read %q.", job.OutputFile)
		}
		return tools.ToolResult{Text: text, Status: tools.ResultBackground, OutputFile: job.OutputFile}, job, nil
	}
	if strings.TrimSpace(args.KillJobID) != "" {
		res, err := c.Exec.Dispatch(ctx, "bash", tc.Function.Arguments)
		return res, nil, err
	}
	if (args.Background || c.Exec.BackgroundThreshold > 0) && c.resources != nil {
		if err := c.resources.tracker.CheckJobLaunch(c.Exec.RunningWorkspaceJobs()); err != nil {
			c.resources.failure = err
			return tools.ToolResult{}, nil, err
		}
	}
	if args.Background {
		job, err := c.Exec.BashBackground(ctx, args)
		if err != nil {
			return tools.ToolResult{}, nil, err
		}
		return backgrounded(job)
	}
	if c.Exec.BackgroundThreshold <= 0 {
		res, err := c.Exec.Dispatch(ctx, "bash", tc.Function.Arguments)
		return res, nil, err
	}
	job, err := c.Exec.BashForeground(ctx, args)
	if err != nil {
		return tools.ToolResult{}, nil, err
	}
	deliver := func() (tools.ToolResult, *tools.Job, error) {
		r, _ := job.Result()
		c.Exec.ForgetJob(job)
		status := tools.ResultSuccess
		if r.Err != nil {
			status = tools.ResultError
		}
		return tools.ToolResult{Text: r.Output, Status: status, ExitCode: &r.ExitCode, OutputFile: job.OutputFile}, nil, r.Err
	}
	timer := time.NewTimer(c.Exec.BackgroundThreshold)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		job.Cancel()
		<-job.Done()
		return deliver()
	case <-job.Done():
		return deliver()
	case <-timer.C:
		if job.Status() != tools.JobRunning {
			return deliver()
		}
		job.Detach()
		return backgrounded(job)
	}
}

// jobResultMessage renders a finished background job the way
// drainPending does: exit code, error if any, and capped output.
// It is a user message, never a second tool message for the same
// call id — the API rejects that with 400.
func jobResultMessage(job *tools.Job, res tools.JobResult) (string, error) {
	res.Output = strings.ToValidUTF8(res.Output, "\uFFFD")
	if res.Err != nil {
		// A timeout kill is not a failure of the command itself:
		// say so, and point at the output file (a killed dev
		// server's partial output is still worth reading).
		if tools.IsTimeout(res.Err) {
			if job.OutputFile != "" {
				return fmt.Sprintf("Background job %s was killed by its timeout: %v. Its partial output is in %s.\nOutput:\n%s", job.ID, res.Err, job.OutputFile, capText(res.Output, 32*1024)), res.Err
			}
			return fmt.Sprintf("Background job %s was killed by its timeout: %v.\nOutput:\n%s", job.ID, res.Err, capText(res.Output, 32*1024)), res.Err
		}
		if res.ExitCodeUnknown {
			return fmt.Sprintf("Background job %s finished (%v; exit status unavailable).\nOutput:\n%s", job.ID, res.Err, capText(res.Output, 32*1024)), res.Err
		}
		return fmt.Sprintf("Background job %s finished with error: %v (exit %d).\nOutput:\n%s", job.ID, res.Err, res.ExitCode, capText(res.Output, 32*1024)), res.Err
	}
	if res.ExitCodeUnknown {
		return fmt.Sprintf("Background job %s finished (exit status unavailable).\nOutput:\n%s", job.ID, capText(res.Output, 32*1024)), nil
	}
	return fmt.Sprintf("Background job %s finished (exit %d).\nOutput:\n%s", job.ID, res.ExitCode, capText(res.Output, 32*1024)), nil
}

// collectFinishedJobs appends a user message for each pending job
// that finished since the last step, in completion order, and drops
// them from pending. It never blocks: a still-running job stays
// pending for a later step or the turn-end drain.
func (c *Config) collectFinishedJobs(msgs []llm.Message, pending []pendingJob) ([]llm.Message, []pendingJob) {
	var done, rest []pendingJob
	var unknownJobs, adoptedNoticeJobs []*tools.Job
	cutoff := time.Now()
	if c.Exec != nil {
		for _, job := range c.Exec.Jobs() {
			if !job.Delivered() && !slices.ContainsFunc(pending, func(p pendingJob) bool { return p.job == job }) {
				pending = append(pending, pendingJob{callID: job.ToolCallID, job: job})
			}
		}
	}
	for _, p := range pending {
		switch p.job.Status() {
		case tools.JobUnknown:
			if !p.job.Delivered() {
				unknownJobs = append(unknownJobs, p.job)
			}
			continue
		case tools.JobDone:
			if p.job.Delivered() {
				continue
			}
			if finished, ok := p.job.FinishedAt(); ok && !finished.After(cutoff) {
				done = append(done, p)
			} else if !p.job.Reported() {
				rest = append(rest, p)
			}
		default: // JobRunning
			if p.job.Adopted && !p.job.AdoptionNoticed() {
				adoptedNoticeJobs = append(adoptedNoticeJobs, p.job)
			}
			if !p.job.Reported() {
				// Already-reported running jobs remain only in the registry:
				// they must not force another drain or discard a final answer.
				rest = append(rest, p)
			}
		}
	}
	for _, job := range unknownJobs {
		res, _ := job.UnknownResult()
		msg := unknownJobMessage(job, res)
		msgs = append(msgs, msg)
		if c.Session != nil {
			c.persist(msgs[len(msgs)-1])
		}
		if c.persistErr != nil {
			return msgs, pending
		}
		job.MarkDelivered()
		if c.progress != nil {
			c.progress.jobs[job.ID] = true
		}
		c.emit(Event{Kind: EventToolBackgroundDone, ToolName: "bash", ToolOutput: msg.Content, ToolErr: res.Err, JobID: job.ID, ToolCallID: job.ToolCallID, InvocationID: job.InvocationID, ToolOutputFile: job.OutputFile})
	}
	for _, job := range adoptedNoticeJobs {
		msg := adoptedJobNoticeMessage(job)
		msgs = append(msgs, msg)
		if c.Session != nil {
			c.persist(msgs[len(msgs)-1])
		}
		if c.persistErr != nil {
			return msgs, pending
		}
		job.MarkAdoptionNoticed()
		job.MarkReported()
		rest = slices.DeleteFunc(rest, func(p pendingJob) bool { return p.job == job })
	}
	if len(done) == 0 {
		return msgs, rest
	}
	sort.Slice(done, func(i, j int) bool {
		ti, _ := done[i].job.FinishedAt()
		tj, _ := done[j].job.FinishedAt()
		return ti.Before(tj)
	})
	for _, p := range done {
		res, _ := p.job.Result()
		obs, toolErr := jobResultMessage(p.job, res)
		obs = c.progressJob(p.job, res, obs)
		msgs = append(msgs, pendingStopCompletion(msgs, p.job, res, obs))
		if c.Session != nil {
			c.persist(msgs[len(msgs)-1])
		}
		if c.persistErr != nil {
			return msgs, pending
		}
		p.job.MarkDelivered()
		c.emit(backgroundDoneEvent(p.job, res, obs, toolErr))
	}
	return msgs, rest
}

// pendingJob is one backgrounded bash call waiting for its result.
// A slice, not a map keyed by call id: two calls must never share an
// entry even if a provider ever reuses a call id.
type pendingJob struct {
	callID string
	job    *tools.Job
}

type jobDelivery struct {
	job    *tools.Job
	result tools.JobResult
	output string
}

// A stop can finish alongside other jobs. Persist every ready outcome in
// completion order before the next tool can produce a newer observation.
func (c *Config) stoppedJobDeliveries() []jobDelivery {
	var deliveries []jobDelivery
	cutoff := time.Now()
	for _, job := range c.Exec.Jobs() {
		if result, done := job.Result(); done && !job.Delivered() {
			if finished, _ := job.FinishedAt(); !finished.After(cutoff) {
				deliveries = append(deliveries, jobDelivery{job: job, result: result})
			}
		}
	}
	sort.SliceStable(deliveries, func(i, j int) bool {
		iTime, _ := deliveries[i].job.FinishedAt()
		jTime, _ := deliveries[j].job.FinishedAt()
		return iTime.Before(jTime)
	})
	for i := range deliveries {
		d := &deliveries[i]
		d.output, _ = jobResultMessage(d.job, d.result)
		d.output = c.progressJob(d.job, d.result, d.output)
	}
	return deliveries
}

// drainTimeout returns the bound for one drainPending wait
// (0 = default 30s). 30s is long enough for a job in its final
// seconds to finish — builds and tests usually land within seconds
// of the model stopping — and short enough that a hung turn ends
// promptly. The bound is paid at most once per job: a job already
// reported still-running is never waited on again (see below).
func (c *Config) drainTimeout() time.Duration {
	if c.DrainTimeout > 0 {
		return c.DrainTimeout
	}
	return 30 * time.Second
}

// drainPending waits for the jobs still running and feeds each result
// back as a user message (appended to msgs and the session), then
// returns the extended msgs. The result is a user message, NOT a
// second tool message for the same call id: the call already has its
// tool result (the "still running" observation), and a second one
// with the same id is a 400 from the API (verified live, P2-8). The
// output is capped like a recent tool result: the fed-back message is
// a user message, so truncateToolResults never trims it.
//
// The wait is BOUNDED (see drainTimeout): a job that never finishes
// — a dev server, a watcher — must not block turn end forever (the
// café deadlock). After the bound the turn ends with the jobs still
// running, and a user message lists them with their output file
// paths, so the model can read partial output. Their results surface
// at the next turn: Run re-attaches unfinished jobs from the
// registry, and collectFinishedJobs delivers them when they finish.
// A job already reported still-running is not waited on again.
func (c *Config) drainPending(ctx context.Context, msgs []llm.Message, pending []pendingJob) ([]llm.Message, error) {
	jobs := make([]*tools.Job, 0, len(pending))
	jobIDs := make([]string, 0, len(pending))
	for _, p := range pending {
		jobs = append(jobs, p.job)
		jobIDs = append(jobIDs, p.job.ID)
	}
	// Deliver jobs that already finished, in completion order.
	// Jobs already reported still-running in an earlier turn are
	// never waited on again: the model already knows about them,
	// so they must not make the next turn pay the bound too.
	var freshJobs []*tools.Job
	for _, job := range jobs {
		if job.Status() == tools.JobRunning && !job.Reported() {
			freshJobs = append(freshJobs, job)
		}
	}
	// Completion callbacks may release another job after a collector's
	// cutoff. Rescan after each delivery batch before classifying unfinished
	// work or returning to a provider request. Each pass delivers new jobs.
	collectReady := func() {
		for {
			before := len(msgs)
			msgs, _ = c.collectFinishedJobs(msgs, pending)
			if c.persistErr != nil || len(msgs) == before {
				return
			}
		}
	}
	collectReady()
	if c.persistErr != nil {
		return msgs, c.persistErr
	}
	freshJobs = slices.DeleteFunc(freshJobs, func(job *tools.Job) bool { return job.Delivered() })
	if len(freshJobs) == 0 || ctx.Err() != nil {
		// Nothing new to wait for: the only unfinished jobs are
		// ones the model was already told about.
		return msgs, ctx.Err()
	}
	c.emit(Event{Kind: EventBackgroundWait, Text: strings.Join(jobIDs, ", ")})
	// Deliver in completion order, not submission order: a slow
	// job must not hold back results that are already ready. One
	// goroutine per job signals a shared channel when its job
	// finishes.
	drainStop := make(chan struct{})
	defer close(drainStop)
	done := make(chan *tools.Job, len(freshJobs))
	for _, job := range freshJobs {
		go func(j *tools.Job) {
			select {
			case <-j.Done():
				done <- j
			case <-drainStop:
			}
		}(job)
	}
	bound := c.drainTimeout()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	remaining := len(freshJobs)
	for remaining > 0 {
		select {
		case <-done:
			collectReady()
			if c.persistErr != nil {
				return msgs, c.persistErr
			}
			remaining--
		case <-timer.C:
			collectReady()
			if c.persistErr != nil {
				return msgs, c.persistErr
			}
			// Bound reached: end the turn with the jobs still
			// running instead of waiting forever. Report each
			// one with its output file path so the model can
			// read partial output; mark them reported so the
			// next turn's drain does not wait on them again.
			var still []*tools.Job
			for _, job := range freshJobs {
				if !job.Delivered() {
					still = append(still, job)
				}
			}
			if len(still) > 0 {
				var b strings.Builder
				fmt.Fprintf(&b, "Background jobs still running after the %s drain bound; the turn ends with them running. Their results will be delivered when they finish.", bound)
				for _, job := range still {
					job.MarkReported()
					if job.OutputFile != "" {
						fmt.Fprintf(&b, "\n- %s (job %s): read its partial output so far with read %q.", job.Command, job.ID, job.OutputFile)
					} else {
						fmt.Fprintf(&b, "\n- %s (job %s): still running.", job.Command, job.ID)
					}
				}
				var snapshots []checkpoint.Job
				for _, job := range still {
					snapshots = append(snapshots, snapshotJob(job, false))
				}
				msgs = append(msgs, llm.Message{Role: "user", Kind: "job", Content: b.String(), Observation: &llm.ToolObservation{Tool: "bash", Jobs: snapshots}})
				if c.Session != nil {
					c.persist(msgs[len(msgs)-1])
				}
			}
			return msgs, ctx.Err()
		case <-ctx.Done():
			collectReady()
			if c.persistErr != nil {
				return msgs, c.persistErr
			}
			return msgs, ctx.Err()
		}
	}
	return msgs, ctx.Err()
}

// MaxStopBlocks caps consecutive turn-end vetoes by stop hooks,
// after Claude Code's Stop hook (8): after that many blocks the
// turn is allowed to end anyway, so a broken check cannot trap the
// model forever.
const MaxStopBlocks = 8

// MaxFailedFixAttempts bounds failed tool attempts without proven state change
// or a successful rerun of a previously failing invocation.
const MaxFailedFixAttempts = 3

// maxLLMAttempts returns the per-step LLM attempt budget.
func (c *Config) maxLLMAttempts() int {
	if c.MaxLLMAttempts <= 0 {
		return 4
	}
	return c.MaxLLMAttempts
}

// retryDelay is the backoff before attempt `attempt+1`, after
// attempt `attempt` failed: exponential 2s, 4s, 8s, capped at 16s.
// A Retry-After delay from the provider replaces the backoff,
// capped at 60s.
func retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > 60*time.Second {
			return 60 * time.Second
		}
		return retryAfter
	}
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 16*time.Second {
		return 16 * time.Second
	}
	return d
}

// sleepCtx sleeps for d unless the context dies first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// streamStep wraps Client.Stream with bounded retries of transient
// LLM failures (a flapping gateway's 403/429/5xx, a dropped
// connection, a stream cut off mid-reply): one failed call must not
// end the turn. It returns the number of attempts used and the final
// error (nil on success). It never retries a cancelled context or a
// non-retryable error, and each retry is announced with an
// EventStepRetry. A completed message is never replayed: once onDone
// has run (doneSeen), the reply already exists, so the call is not
// retried even if it then errors. A stream cut off after partial
// output IS retried: the partial reply is discarded first — an
// EventStepDiscard tells every renderer to drop what it showed for
// this step, and onRetry resets the caller's per-attempt state —
// then the call is replayed from scratch, so the GUI never shows
// the half-reply followed by the replay.
func (c *Config) streamStep(ctx context.Context, operation string, apiMsgs []llm.Message, tools []llm.ToolDef, onText func(string), onDone func(llm.Message, llm.Usage), onThinking func(string), onRetry func()) (int, llm.Usage, error) {
	c.stepID = rand.Text()
	attempts := c.maxLLMAttempts()
	var err error
	var spent llm.Usage
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := c.budgetRequest(); err != nil {
			return attempt - 1, spent, err
		}
		requestCtx := c.requestContext(ctx, operation, attempt, &spent)
		outputSeen := false
		doneSeen := false
		wrappedText := func(s string) {
			if onText != nil {
				outputSeen = true
				onText(s)
			}
		}
		wrappedDone := func(m llm.Message, u llm.Usage) {
			outputSeen = true
			doneSeen = true
			if onDone != nil {
				onDone(m, u)
			}
		}
		wrappedThinking := func(s string) {
			if onThinking != nil {
				outputSeen = true
				onThinking(s)
			}
		}
		err = c.Client.Stream(requestCtx, apiMsgs, tools, wrappedText, wrappedDone, wrappedThinking)
		if c.persistErr != nil {
			return attempt, spent, c.persistErr
		}
		if budgetErr := c.budgetError(); budgetErr != nil {
			return attempt, spent, budgetErr
		}
		if err == nil {
			return attempt, spent, nil
		}
		contextOverflow := llm.IsContextLengthExceeded(err)
		retry := !doneSeen && !contextOverflow && ctx.Err() == nil && llm.IsRetryable(err) && attempt < attempts
		var protocolErr *llm.ProtocolError
		if !doneSeen && outputSeen && (retry || contextOverflow || errors.As(err, &protocolErr)) {
			// The stream was cut off after showing partial output:
			// discard the partial reply before replaying. The
			// assistant message and usage need no reset here: they
			// are only written by onDone, which sets doneSeen and
			// blocks the retry above. Only the caller's thinking
			// accumulator holds per-attempt state, and onRetry
			// resets it.
			next := 0
			if retry {
				next = attempt + 1
			}
			c.emit(Event{Kind: EventStepDiscard, Text: fmt.Sprintf("LLM stream cut off after partial output (%v); discarding the partial reply", err), RetryAttempt: next})
			if onRetry != nil {
				onRetry()
			}
		}
		if !retry {
			return attempt, spent, err
		}
		if budgetErr := c.budgetRequest(); budgetErr != nil {
			return attempt, spent, budgetErr
		}
		delay := retryDelay(attempt, llm.RetryAfter(err))
		c.emit(Event{Kind: EventStepRetry, Text: fmt.Sprintf("%s failed (attempt %d/%d): %v; retrying in %s", operation, attempt, attempts, err, delay), RetryAttempt: attempt + 1})
		if !sleepCtx(ctx, delay) {
			return attempt, spent, err
		}
	}
	return attempts, spent, err
}

// Run executes one user turn. It appends messages to the session store
// and returns the assistant's final text, total tokens used, and error.
func (c *Config) Run(ctx context.Context, history []llm.Message) (string, llm.Usage, error) {
	var task string
	for i := len(history) - 1; i >= 0; i-- {
		if m := history[i]; m.Role == "user" && (m.Kind == "task" || m.Kind == "") {
			task = m.Content
			break
		}
	}
	return c.RunTask(ctx, history, task)
}

// RunTask takes the user's original task separately from agent history so
// context hooks and compaction cannot change the authority used by the reviewer.
func (c *Config) RunTask(ctx context.Context, history []llm.Message, task string) (text string, runUsage llm.Usage, runErr error) {
	c.stepID = ""
	c.persistErr = nil
	c.persistErrEmitted = false
	c.LastTerminalReason = ""
	c.LastTaskOutcome = ""
	c.LastTurnBudget = nil
	c.verifier = nil
	for _, m := range history {
		if m.Kind == "checkpoint" {
			if _, err := checkpoint.Decode([]byte(m.Content)); err != nil {
				c.LastTerminalReason = TerminalError
				err = fmt.Errorf("invalid historical checkpoint: %w", err)
				c.emit(Event{Kind: EventError, Err: err, TerminalReason: TerminalError})
				return "", llm.Usage{}, err
			}
		}
	}
	if c.Verification != nil && len(c.Verification.Required) > 0 {
		if err := c.Verification.Validate(); err != nil {
			c.LastTerminalReason = TerminalError
			c.emit(Event{Kind: EventError, Err: err, TerminalReason: TerminalError})
			return "", llm.Usage{}, err
		}
		c.verifier = &verificationRun{policy: c.Verification, latest: map[string]verification.Evidence{}}
	}
	defer func() {
		if c.persistErr != nil {
			runErr = c.persistErr
			c.LastTerminalReason = TerminalError
			if !c.persistErrEmitted {
				c.emit(Event{Kind: EventError, Err: runErr, Text: text, TerminalReason: TerminalError})
			}
		}
	}()
	budgetCtx, cancelBudget, budgetErr := c.startBudget(ctx)
	defer cancelBudget()
	defer c.finishBudget(&text, &runErr)
	ctx = budgetCtx
	if budgetErr != nil {
		c.LastTerminalReason = TerminalError
		c.emit(Event{Kind: EventError, Err: budgetErr, TerminalReason: TerminalError})
		return "", llm.Usage{}, budgetErr
	}
	steps := c.MaxSteps
	// MaxSteps <= 0 means unlimited (the mainstream default): the turn
	// runs until the model stops calling tools, an error ends it, or
	// the task is blocked. The bounds are then auto-compaction at 70%
	// of the context window, finite turn resource budgets, and the
	// doom-loop / blocked guards. A
	// positive MaxSteps opts into a hard cap with the grace summary.
	hasCap := steps > 0
	guidanceState := newGuidanceTurn(c.Exec)
	reviewIntent := approvalIntent{Task: task}
	msgs := append([]llm.Message{{Role: "system", Content: c.System + verificationInstructions(c.Verification)}}, history...)
	msgs = WithoutHistoricalGuidance(msgs)
	receiveSteering := func(finishing bool, remainingCalls []llm.ToolCall, startedInvocation ...string) bool {
		if c.ReceiveSteering == nil || ctx.Err() != nil {
			return false
		}
		received := false
		c.ReceiveSteering(finishing, func(text string) bool {
			if ctx.Err() != nil || !c.canRequestForSteering() {
				return false
			}
			if len(remainingCalls) > 0 {
				results := c.rejectBudgetTools(remainingCalls, errors.New("the user updated the task before these calls ran"), startedInvocation...)
				msgs = append(msgs, results...)
				remainingCalls = nil
			}
			msg := llm.Message{Role: "user", Kind: "steering", Content: text}
			c.persist(msg)
			if c.persistErr != nil {
				return false
			}
			msgs = append(msgs, msg)
			reviewIntent.Updates = append(reviewIntent.Updates, text)
			received = true
			return true
		})
		return received
	}
	var total llm.Usage
	var lastText string
	// pending background bash jobs, in call order. Their results are
	// fed back as user messages when the turn would otherwise end
	// (see drainPending).
	var pending []pendingJob
	c.progress = &turnProgress{ctx: ctx, jobs: make(map[string]bool)}
	defer func() { c.progress = nil }()
	if c.Exec != nil {
		sessID := ""
		if c.Session != nil {
			sessID = c.Session.ID
		}
		if !c.Exec.SessionRecovered(sessID) {
			source := msgs
			if c.Session != nil {
				if saved, err := session.Load(c.Session.ID); err == nil {
					source = saved
				}
			}
			if _, err := RecoverExecutorJobs(c.Exec, sessID, source); err != nil {
				c.LastTerminalReason = TerminalError
				c.emit(Event{Kind: EventError, Err: err, TerminalReason: TerminalError})
				return "", total, err
			}
		}
	}
	if err := c.reconcileJobDeliveries(); err != nil {
		c.LastTerminalReason = TerminalError
		c.emit(Event{Kind: EventError, Err: err, TerminalReason: TerminalError})
		return "", total, err
	}
	// Re-attach jobs from earlier turns whose results were never
	// delivered: a job that finished after its turn ended — or
	// after a bounded drain gave up on it — still owes the model
	// its result, and a job still running may finish mid-turn, in
	// which case collectFinishedJobs delivers it at the next step
	// boundary. Delivered jobs are not re-attached (their result
	// is already in history), and a job already reported
	// still-running is never waited on again by the drain.
	if c.Exec != nil {
		for _, job := range c.Exec.Jobs() {
			if !job.Delivered() {
				pending = append(pending, pendingJob{callID: "", job: job})
			} else {
				// This outcome already belongs to an earlier turn.
				c.progress.jobs[job.ID] = true
			}
		}
	}
	stopBlocks := 0
	warnOnDoomLoop := c.Headless && (c.ConfirmContinuation == nil || c.DenyInsteadOfPrompt)
	finishBlocked := func() (string, llm.Usage, error) {
		blocked := fmt.Errorf("task blocked: %d tool attempts failed without proven state or result progress", MaxFailedFixAttempts)
		if ctx.Err() == nil {
			graceText, graceUsage, ok := c.graceCall(ctx, msgs, blockedGracePrompt(blocked, c.progress.lastErrors), guidanceState)
			c.addObservedUsage(&total, graceUsage)
			if ok {
				lastText = graceText
			}
		}
		if err := ctx.Err(); err != nil {
			c.LastTerminalReason = TerminalAbort
			c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
			return lastText, total, err
		}
		c.LastTerminalReason = TerminalBlocked
		c.emit(Event{Kind: EventError, Err: blocked, Text: lastText, Usage: total, TerminalReason: TerminalBlocked})
		return lastText, total, blocked
	}

	for step := 0; !hasCap || step < steps; step++ {
		canContinue := !hasCap || step+1 < steps
		receiveSteering(false, nil)
		// Finished background jobs report in at the next step,
		// in completion order — the model sees results as soon
		// as they are ready, not only when the turn would end.
		msgs, pending = c.collectFinishedJobs(msgs, pending)
		if c.persistErr != nil {
			return lastText, total, c.persistErr
		}
		if guidanceState != nil {
			msgs = guidanceState.refreshRoot(ctx, c, msgs)
		}
		if c.persistErr != nil {
			return lastText, total, c.persistErr
		}
		// Rendering uses a fixed per-result cap. Old content changes only
		// at a persisted checkpoint when the context budget is reached.
		if c.ContextLimit > 0 && c.needsCompact(msgs) {
			var prepare *checkpointPreparation
			if guidanceState != nil {
				prepare = &checkpointPreparation{guidance: guidanceState}
			}
			compacted, usage, err := c.compactInLoop(ctx, msgs, prepare)
			c.addObservedUsage(&total, usage)
			c.emit(Event{Kind: EventStepUsage, Usage: usage})
			if err != nil {
				c.LastTerminalReason = TerminalError
				if ctx.Err() != nil {
					c.LastTerminalReason = TerminalAbort
				} else {
					lastText = stoppedReport(lastText, err)
				}
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: c.LastTerminalReason})
				return lastText, total, err
			}
			msgs = compacted
			if c.needsCompact(msgs) {
				err := fmt.Errorf("context still exceeds the budget after compaction; reduce the task, system prompt or tool definitions, or select a larger context window")
				c.LastTerminalReason = TerminalError
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: TerminalError})
				return lastText, total, err
			}
		}
		result, apiMsgs, msgsAfterStep, err := c.streamTurnStep(ctx, msgs, guidanceState, &total)
		msgs = msgsAfterStep
		assistant, usage, attempts := result.assistant, result.usage, result.attempts
		if err != nil {
			wrapped := fmt.Errorf("step %d: %w", step+1, err)
			if attempts > 1 {
				wrapped = fmt.Errorf("step %d: %w (after %d attempts)", step+1, err, attempts)
			}
			// The turn is over, but a running job's result is still
			// the user's data: deliver it before reporting the error
			// so it is not silently lost.
			if len(pending) > 0 && c.budgetError() == nil {
				_, _ = c.drainPending(ctx, msgs, pending)
			}
			// A cancelled context is an abort, not a provider error:
			// the RPC server reports it as reason "abort" with no
			// error string (see TerminalAbort).
			c.LastTerminalReason = TerminalError
			if ctx.Err() != nil {
				c.LastTerminalReason = TerminalAbort
			} else {
				lastText = stoppedReport(lastText, wrapped)
			}
			c.emit(Event{Kind: EventError, Err: wrapped, Text: lastText, TerminalReason: c.LastTerminalReason})
			return lastText, total, wrapped
		}
		var offeredGuidance map[llm.GuidanceCursor]bool
		if guidanceState != nil {
			guidanceState.unread = nil // the completed request received its result
			offeredGuidance = guidanceOffers(apiMsgs)
		}
		c.LastPromptTokens = usage.PromptTokens
		c.lastPromptEstimate = estimatePromptTokens(apiMsgs, c.Tools)
		lastText = assistant.Content
		// A job can finish during any provider response, including a final
		// answer. Save the outcome before the newer batch; a final answer
		// that has not seen it needs another model step to account for it.
		beforeCollection := len(msgs)
		msgs, pending = c.collectFinishedJobs(msgs, pending)
		collectedAfterResponse := len(msgs) > beforeCollection
		if c.persistErr != nil {
			return lastText, total, c.persistErr
		}
		assistantIdx := len(msgs)
		msgs = append(msgs, assistant)
		if c.Session != nil {
			c.persist(assistant)
		}
		if c.persistErr != nil {
			return lastText, total, c.persistErr
		}
		if assistant.FinishReason == "length" || assistant.FinishReason == "max_tokens" || assistant.FinishReason == "incomplete" {
			err := fmt.Errorf("model output stopped at its token limit (%s); response is incomplete", assistant.FinishReason)
			c.LastTerminalReason = TerminalOutputLimit
			c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: TerminalOutputLimit})
			return lastText, total, err
		}
		if canContinue && receiveSteering(false, assistant.ToolCalls) {
			continue
		}
		if c.persistErr != nil {
			return lastText, total, c.persistErr
		}
		if len(assistant.ToolCalls) == 0 {
			if collectedAfterResponse {
				if c.progress.failures >= MaxFailedFixAttempts && len(pending) > 0 {
					var err error
					msgs, err = c.drainPending(ctx, msgs, pending)
					msgs, pending = c.collectFinishedJobs(msgs, nil)
					if c.persistErr != nil {
						return lastText, total, c.persistErr
					}
					if err == nil {
						err = ctx.Err()
					}
					if err != nil {
						c.LastTerminalReason = TerminalAbort
						c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
						return lastText, total, err
					}
				}
				if c.progress.failures >= MaxFailedFixAttempts && ctx.Err() == nil {
					return finishBlocked()
				}
				followup := llm.Message{Role: "user", Kind: "job", Content: "A background job completed while the preceding answer was generated. Reconsider that answer using the saved outcome above."}
				msgs = append(msgs, followup)
				c.persist(followup)
				if c.persistErr != nil {
					return lastText, total, c.persistErr
				}
				continue
			}
			if len(pending) == 0 {
				if c.progress.failures >= MaxFailedFixAttempts && ctx.Err() == nil {
					return finishBlocked()
				}
				if assistant.Refusal != "" || assistant.FinishReason == "refusal" || assistant.FinishReason == "content_filter" || strings.TrimSpace(lastText) == "" {
					message := "model returned an empty answer without tool calls"
					if assistant.Refusal != "" || assistant.FinishReason == "refusal" || assistant.FinishReason == "content_filter" {
						message = "model refused the request"
						if assistant.Refusal != "" {
							message += ": " + assistant.Refusal
						}
					}
					err := fmt.Errorf("%s", message)
					c.LastTerminalReason = TerminalError
					c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: TerminalError})
					return lastText, total, err
				}
				// Turn-end gate (Claude's Stop hook): a plugin stop
				// hook may veto the end — e.g. a check the final
				// answer failed. The reason is fed back to the
				// model as a user message and the turn continues,
				// up to MaxStopBlocks feedback rounds. A further veto stops
				// with an incomplete outcome, never a successful end_turn.
				if ctx.Err() == nil && c.Exec != nil && c.Exec.Plugins != nil {
					c.verificationTouch(ctx)
					sessionID := ""
					if c.Session != nil {
						sessionID = c.Session.ID
					}
					if reason := c.Exec.Plugins.RunStopHooks(ctx, sessionID, lastText); reason != "" {
						if stopBlocks >= MaxStopBlocks {
							err := fmt.Errorf("stop hook veto cap reached (%d); task incomplete: %s", MaxStopBlocks, reason)
							c.persist(llm.Message{Role: "user", Kind: "task_outcome", Content: "[harness task outcome]\nTask incomplete: the stop hook veto limit was reached. Outstanding issue: " + reason})
							c.LastTerminalReason = TerminalStopCap
							c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalStopCap})
							return lastText, total, err
						}
						stopBlocks++
						c.emit(Event{Kind: EventStopBlocked, Text: reason})
						blockMsg := llm.Message{Role: "user", Kind: "hook", Content: "A stop hook blocked the turn from ending: " + reason + " Address it before finishing, then answer again."}
						msgs = append(msgs, blockMsg)
						if c.Session != nil {
							c.persist(blockMsg)
						}
						continue
					}
				}
				outcome, missing := c.verificationGate(ctx)
				if err := ctx.Err(); err != nil {
					c.LastTerminalReason = TerminalAbort
					c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
					return lastText, total, err
				}
				if missing != "" {
					if c.verifier.feedback >= maxVerificationFeedback {
						err := fmt.Errorf("required verification failed: %s", missing)
						c.persist(llm.Message{Role: "user", Kind: "task_outcome", Content: "[harness task outcome]\nVerification failed: " + missing})
						c.LastTerminalReason = TerminalVerificationFailed
						c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalVerificationFailed, TaskOutcome: OutcomeVerificationFailed})
						return lastText, total, err
					}
					c.verifier.feedback++
					msg := llm.Message{Role: "user", Kind: "verification", Content: "[harness required verification]\n" + missing}
					msgs = append(msgs, msg)
					c.persist(msg)
					c.emit(Event{Kind: EventStopBlocked, Text: missing})
					continue
				}
				if canContinue && receiveSteering(true, nil) {
					continue
				}
				if c.persistErr != nil {
					return lastText, total, c.persistErr
				}
				c.LastTerminalReason = TerminalEndTurn
				c.emit(Event{Kind: EventDone, Text: lastText, Usage: total, TerminalReason: TerminalEndTurn, TaskOutcome: outcome})
				return lastText, total, nil
			}
			// The model stopped calling tools but background jobs are
			// still running: wait for them, feed the results back,
			// and let the model act on them in the next LLM call.
			var err error
			msgs, err = c.drainPending(ctx, msgs, pending)
			pending = nil
			if c.persistErr != nil {
				return lastText, total, c.persistErr
			}
			if err != nil {
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
				return lastText, total, err
			}
			if c.progress.failures >= MaxFailedFixAttempts {
				return finishBlocked()
			}
			continue
		}
		if err := c.reserveBudgetTools(assistant.ToolCalls); err != nil {
			c.rejectBudgetTools(assistant.ToolCalls, err)
			return lastText, total, err
		}
		if c.Exec != nil {
			if target := pendingStopTarget(c.Exec.Jobs()); target != nil {
				msgs = append(msgs, c.rejectBudgetTools(assistant.ToolCalls, fmt.Errorf("background job %s is still finalizing; reconsider these calls after its outcome is recorded", target.ID))...)
				if c.persistErr != nil {
					return lastText, total, c.persistErr
				}
				// The batch is settled without execution. Wait once for its
				// prerequisite instead of spending model steps in a busy loop.
				c.emit(Event{Kind: EventBackgroundWait, Text: target.ID})
				stopWait := min(c.drainTimeout(), 5*time.Second)
				timer := time.NewTimer(stopWait)
				var waitErr error
				select {
				case <-target.Done():
				case <-timer.C:
					waitErr = fmt.Errorf("background job %s is still finalizing after %s; resume after its outcome is available", target.ID, stopWait)
				case <-ctx.Done():
					waitErr = ctx.Err()
				}
				timer.Stop()
				msgs, pending = c.collectFinishedJobs(msgs, pending)
				if c.persistErr != nil {
					return lastText, total, c.persistErr
				}
				if ctx.Err() != nil {
					waitErr = ctx.Err()
				} else if target.Delivered() {
					waitErr = nil
				}
				if waitErr != nil {
					c.LastTerminalReason = TerminalError
					if ctx.Err() != nil {
						c.LastTerminalReason = TerminalAbort
					}
					lastText = "Task incomplete: " + waitErr.Error()
					c.persist(llm.Message{Role: "user", Kind: "task_outcome", Content: lastText})
					c.emit(Event{Kind: EventError, Err: waitErr, Text: lastText, Usage: total, TerminalReason: c.LastTerminalReason})
					return lastText, total, waitErr
				}
				if c.progress.failures >= MaxFailedFixAttempts && len(pending) > 0 {
					var err error
					msgs, err = c.drainPending(ctx, msgs, pending)
					msgs, pending = c.collectFinishedJobs(msgs, nil)
					if c.persistErr != nil {
						return lastText, total, c.persistErr
					}
					if err == nil {
						err = ctx.Err()
					}
					if err != nil {
						c.LastTerminalReason = TerminalAbort
						c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
						return lastText, total, err
					}
				}
				if c.progress.failures >= MaxFailedFixAttempts && ctx.Err() == nil {
					return finishBlocked()
				}
				continue
			}
		}
		// Execute tool calls in order. (Parallel batches are v0.2; serial
		// avoids two tools writing the same file at once.)
		for i := range assistant.ToolCalls {
			if canContinue && receiveSteering(false, assistant.ToolCalls[i:]) {
				break
			}
			tc := &assistant.ToolCalls[i]
			if c.persistErr != nil {
				return lastText, total, c.persistErr
			}
			if err := ctx.Err(); err != nil {
				if c.budgetError() != nil {
					c.rejectBudgetTools(assistant.ToolCalls[i:], err)
				}
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: TerminalAbort})
				return lastText, total, err
			}
			inv := llm.ToolInvocation{ID: rand.Text(), CallIndex: i, ToolCallID: tc.ID, ToolName: tc.Function.Name, OriginalArguments: tc.Function.Arguments}
			recordInvocation := func(args string) error {
				inv.Arguments = args
				if c.Session != nil {
					if err := c.Session.RecordInvocation(inv); err != nil {
						c.persistErr = fmt.Errorf("save tool invocation: %w", err)
						return c.persistErr
					}
				}
				msgs[assistantIdx].ToolInvocations = append(msgs[assistantIdx].ToolInvocations, inv)
				return nil
			}
			startTool := func(args string) {
				c.emit(Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: args, ToolCallID: tc.ID, InvocationID: inv.ID})
			}
			// Plugin tool_call hooks run before approval: a denied
			// call never prompts the user, and the approval flow
			// still applies to anything the hooks let through — a
			// hook can add a gate, never remove one. A rewrite
			// replaces the args the rest of this path sees
			// (approval prompt, audit, dispatch) and updates the
			// in-memory assistant message so subsequent steps stay in sync.
			argsJSON := tc.Function.Arguments
			// A tool the model was not offered in this turn's list
			// is refused outright, before approval: the offered
			// list is the enforcement, not just a hint. Without
			// this, a model that hallucinates a write call from
			// the system prompt (which still lists all seven
			// built-ins) would bypass plan mode — and a call for
			// a disabled plugin's tool would run too. The model
			// gets an error observation, like a hook denial, and
			// the refusal is audited.
			if !toolOffered(c.Tools, tc.Function.Name) {
				// No validated invocation exists; only the request ID is durable.
				inv.ID = ""
				c.progressFailure(CanonicalToolKey(tc.Function.Name, argsJSON), failureguard.PolicyDenied)
				recordLastError(&c.progress.lastErrors, "tool not offered: "+tc.Function.Name)
				obs := fmt.Sprintf("Tool %s is not available in this turn. Do not retry this tool call; explain and propose an alternative.", tc.Function.Name)
				if c.Session != nil {
					_ = c.Session.Audit(tools.AuditEntry{ToolCallID: tc.ID, InvocationID: inv.ID, TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: "tool not offered in this turn"})
				}
				startTool(argsJSON)
				c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, InvocationID: inv.ID, ToolOutput: obs, ToolErr: fmt.Errorf("tool %s not offered in this turn", tc.Function.Name)})
				msgs = append(msgs, llm.Message{Role: "tool", Kind: notExecutedKind, Content: obs, ToolCallID: tc.ID, InvocationID: inv.ID, ToolError: true})
				if c.Session != nil {
					c.persist(msgs[len(msgs)-1])
				}
				continue
			}
			if err := c.validateArgs(tc.Function.Name, argsJSON); err != nil {
				msgs = c.invalidArgs(msgs, *tc, err)
				c.progressFailure(CanonicalToolKey(tc.Function.Name, argsJSON), failureguard.InvalidInput)
				recordLastError(&c.progress.lastErrors, err.Error())
				continue
			}
			c.verificationTouch(ctx)
			if c.Exec != nil && c.Exec.Plugins != nil {
				newArgs, deny := c.Exec.Plugins.RunToolCallHooks(ctx, tc.Function.Name, argsJSON)
				if deny != "" {
					inv.ID = ""
					c.progressFailure(CanonicalToolKey(tc.Function.Name, argsJSON), failureguard.PolicyDenied)
					recordLastError(&c.progress.lastErrors, "plugin denied tool call")
					obs := "Denied by plugin " + deny + ". Do not retry this tool call; explain and propose an alternative."
					if c.Session != nil {
						_ = c.Session.Audit(tools.AuditEntry{ToolCallID: tc.ID, InvocationID: inv.ID, TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: deny})
					}
					startTool(argsJSON)
					c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, InvocationID: inv.ID, ToolOutput: obs, ToolErr: fmt.Errorf("denied by plugin %s", deny)})
					msgs = append(msgs, llm.Message{Role: "tool", Kind: notExecutedKind, Content: obs, ToolCallID: tc.ID, InvocationID: inv.ID, ToolError: true})
					if c.Session != nil {
						c.persist(msgs[len(msgs)-1])
					}
					continue
				}
				if newArgs != argsJSON {
					inv.Rewrites = append(inv.Rewrites, llm.ArgumentRewrite{Source: "plugin_hook", Arguments: newArgs})
				}
				argsJSON = newArgs
				tc.Function.Arguments = argsJSON
				msgs[assistantIdx].ToolCalls[i].Function.Arguments = argsJSON
			}
			if err := c.validateArgs(tc.Function.Name, argsJSON); err != nil {
				msgs = c.invalidArgs(msgs, *tc, err)
				c.progressFailure(CanonicalToolKey(tc.Function.Name, argsJSON), failureguard.InvalidInput)
				recordLastError(&c.progress.lastErrors, err.Error())
				continue
			}
			if bound := c.verificationArguments(tc.Function.Name, argsJSON); bound != argsJSON {
				inv.Rewrites = append(inv.Rewrites, llm.ArgumentRewrite{Source: "verification_cwd", Arguments: bound})
				argsJSON = bound
				tc.Function.Arguments = bound
				msgs[assistantIdx].ToolCalls[i].Function.Arguments = bound
			}
			// Doom-loop guard: an identical call for the fifth
			// consecutive time asks the user (once / session /
			// deny), before any approval prompt — the user must
			// not be asked twice about the same call. A denied
			// call ends the turn with the structured reason.
			useArgs, allowed, contErr := c.checkRepeatedCall(ctx, tc.Function.Name, argsJSON)
			if errors.Is(contErr, ErrApprovalDetached) {
				c.rejectBudgetTools(assistant.ToolCalls[i:], contErr)
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: contErr, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
				return lastText, total, contErr
			}
			if err := ctx.Err(); err != nil {
				c.rejectBudgetTools(assistant.ToolCalls[i:], err)
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
				return lastText, total, err
			}
			if !allowed {
				if err := recordInvocation(argsJSON); err != nil {
					return lastText, total, err
				}
				obs := RepeatedToolCallMessage(tc.Function.Name, RepeatedToolCallThreshold)
				if c.Session != nil {
					_ = c.Session.Audit(tools.AuditEntry{ToolCallID: tc.ID, InvocationID: inv.ID, TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: "repeated tool call denied"})
				}
				startTool(argsJSON)
				c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, InvocationID: inv.ID, ToolOutput: obs, ToolErr: ErrRepeatedToolCallDenied})
				msgs = append(msgs, llm.Message{Role: "tool", Kind: notExecutedKind, Content: obs, ToolCallID: tc.ID, InvocationID: inv.ID, ToolError: true})
				if c.Session != nil {
					c.persist(msgs[len(msgs)-1])
				}
				// In headless mode, give the model one soft tool-error chance to
				// change approach or summarize progress before terminating the turn.
				if warnOnDoomLoop && ctx.Err() == nil {
					warnOnDoomLoop = false
					c.DoomLoop().clearStreak()
					continue
				}
				if len(pending) > 0 {
					_, _ = c.drainPending(ctx, msgs, pending)
				}
				c.LastTerminalReason = TerminalRepeatedToolCallDenied
				denied := fmt.Errorf("%w: %s repeated %d times with identical input", ErrRepeatedToolCallDenied, tc.Function.Name, RepeatedToolCallThreshold)
				c.emit(Event{Kind: EventError, Err: denied, Text: lastText, TerminalReason: TerminalRepeatedToolCallDenied})
				return lastText, total, denied
			}
			if useArgs != argsJSON {
				inv.Rewrites = append(inv.Rewrites, llm.ArgumentRewrite{Source: "continuation", Arguments: useArgs})
			}
			argsJSON = useArgs
			if err := c.validateArgs(tc.Function.Name, argsJSON); err != nil {
				msgs = c.invalidArgs(msgs, *tc, err)
				c.progressFailure(CanonicalToolKey(tc.Function.Name, argsJSON), failureguard.InvalidInput)
				recordLastError(&c.progress.lastErrors, err.Error())
				continue
			}
			guardArgs := argsJSON
			mode := c.ApprovalMode()
			var bashDir *tools.BashDirectory
			if tc.Function.Name == "bash" && mode == approvalmode.ApproveForMe && !c.DenyInsteadOfPrompt {
				var err error
				argsJSON, bashDir, err = c.bindBashReview(guardArgs)
				if err != nil {
					msgs = c.invalidArgs(msgs, *tc, err)
					c.progressFailure(CanonicalToolKey(tc.Function.Name, guardArgs), failureguard.Environment)
					recordLastError(&c.progress.lastErrors, err.Error())
					continue
				}
				if argsJSON != guardArgs {
					inv.Rewrites = append(inv.Rewrites, llm.ArgumentRewrite{Source: "approval_cwd", Arguments: argsJSON})
				}
			}
			tc.Function.Arguments = argsJSON
			msgs[assistantIdx].ToolCalls[i].Function.Arguments = argsJSON
			var mutation *tools.Mutation
			if tc.Function.Name == "write" || tc.Function.Name == "edit" {
				var target struct {
					Path string `json:"path"`
				}
				_ = json.Unmarshal([]byte(argsJSON), &target)
				var err error
				mutation, err = c.Exec.PrepareMutation(target.Path)
				if err != nil {
					msgs = c.invalidArgs(msgs, *tc, err)
					c.progressFailure(CanonicalToolKey(tc.Function.Name, argsJSON), failureguard.Environment)
					recordLastError(&c.progress.lastErrors, err.Error())
					continue
				}
				inv.TargetPath = mutation.TargetPath
			}
			closeMutation := func() {
				if mutation != nil {
					_ = mutation.Close()
				}
			}
			if mutation != nil && guidanceState != nil {
				if update, err := guidanceState.forMutation(ctx, mutation); update != nil || err != nil {
					closeMutation()
					msgs = c.deferForGuidance(msgs, assistant.ToolCalls[i:], "", update, err)
					break
				}
			}
			if err := recordInvocation(argsJSON); err != nil {
				closeMutation()
				return lastText, total, err
			}
			startTool(argsJSON)
			callKey := CanonicalToolKey(tc.Function.Name, argsJSON)
			// The guard admits a bounded retry only with proven changed state.
			// Check before approval, and keep one immutable key for async results.
			decision := c.progressAdmit(callKey)
			if err := ctx.Err(); err != nil {
				closeMutation()
				c.rejectBudgetTools(assistant.ToolCalls[i:], err, inv.ID)
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
				return lastText, total, err
			}
			if decision.Denied {
				failures := decision.Failures
				obs := RepeatedFailureDeniedMessage(tc.Function.Name, failures)
				if c.Session != nil {
					_ = c.Session.Audit(tools.AuditEntry{ToolCallID: tc.ID, InvocationID: inv.ID, TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: "repeated tool failure denied"})
				}
				c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, InvocationID: inv.ID, ToolOutput: obs, ToolErr: ErrRepeatedToolFailureDenied})
				msgs = append(msgs, llm.Message{Role: "tool", Kind: notExecutedKind, Content: obs, ToolCallID: tc.ID, InvocationID: inv.ID, ToolError: true})
				if c.Session != nil {
					c.persist(msgs[len(msgs)-1])
				}
				c.progress.failures++
				recordLastError(&c.progress.lastErrors, obs)
				closeMutation()
				continue
			}
			req := ApprovalRequest{ToolName: tc.Function.Name, Arguments: tc.Function.Arguments, ToolCallID: tc.ID, InvocationID: inv.ID, TargetPath: inv.TargetPath, Protected: mutation != nil && mutation.Protected}
			if c.ApprovalRequired(mode, req) {
				ok, approver, approvalNote, err := c.approve(ctx, mode, req, reviewIntent, &total)
				// Audit every decision (approved, denied, or errored)
				// to the session sidecar, for later review.
				if c.Session != nil {
					entry := tools.AuditEntry{ToolCallID: tc.ID, InvocationID: inv.ID, TS: time.Now(), Kind: "approval", Tool: req.ToolName, Args: truncate(req.Arguments, 500), ExitCode: -1, Approver: approver, Reason: approvalNote}
					if err != nil {
						entry.Error = err.Error()
					} else if ok {
						entry.Decision = "approved"
					} else {
						entry.Decision = "denied"
					}
					_ = c.Session.Audit(entry)
				}
				if err != nil {
					if approver == "reviewer" || c.budgetError() != nil {
						c.rejectBudgetTools(assistant.ToolCalls[i:], err, inv.ID)
					}
					if errors.Is(err, ErrApprovalDetached) || ctx.Err() != nil {
						c.LastTerminalReason = TerminalAbort
					} else {
						c.LastTerminalReason = TerminalError
					}
					c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: c.LastTerminalReason})
					closeMutation()
					return lastText, total, err
				}
				if !ok {
					c.progressFailure(callKey, failureguard.PolicyDenied)
					obs := "User denied this tool call. Do not retry it; explain and propose an alternative."
					if approvalNote != "" {
						obs = approvalNote + " Do not retry this tool call; explain and propose an alternative."
					}
					recordLastError(&c.progress.lastErrors, obs)
					c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, InvocationID: inv.ID, ToolOutput: obs, ToolErr: fmt.Errorf("tool call denied by %s", approver)})
					msgs = append(msgs, llm.Message{Role: "tool", Kind: notExecutedKind, Content: obs, ToolCallID: tc.ID, InvocationID: inv.ID, ToolError: true})
					if c.Session != nil {
						c.persist(msgs[len(msgs)-1])
					}
					closeMutation()
					continue
				}
			}
			if err := ctx.Err(); err != nil {
				closeMutation()
				c.rejectBudgetTools(assistant.ToolCalls[i:], err, inv.ID)
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
				return lastText, total, err
			}
			if mutation != nil && guidanceState != nil {
				if update, err := guidanceState.forMutation(ctx, mutation); update != nil || err != nil {
					closeMutation()
					msgs = c.deferForGuidance(msgs, assistant.ToolCalls[i:], inv.ID, update, err)
					break
				}
			}
			toolCtx := tools.WithFailureKey(tools.WithInvocation(ctx, inv.ID, tc.ID), callKey)
			if tc.Function.Name == tools.GuidanceTool {
				toolCtx = tools.WithGuidanceOffers(toolCtx, func(cursor llm.GuidanceCursor) bool { return offeredGuidance[cursor] })
			}
			if mutation != nil {
				toolCtx = tools.WithMutation(toolCtx, mutation)
			}
			if bashDir != nil {
				toolCtx = tools.WithBashDirectory(toolCtx, bashDir)
			}
			attempt := c.verificationStart(toolCtx, tc.Function.Name, argsJSON)
			if attempt != nil {
				attempt.invocationID, attempt.toolCallID = inv.ID, tc.ID
			}
			if err := ctx.Err(); err != nil {
				closeMutation()
				c.rejectBudgetTools(assistant.ToolCalls[i:], err, inv.ID)
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
				return lastText, total, err
			}
			// Approval and preparation can outlast the user's original direction.
			steered := canContinue && receiveSteering(false, assistant.ToolCalls[i:], inv.ID)
			if c.persistErr != nil {
				closeMutation()
				return lastText, total, c.persistErr
			}
			if steered {
				closeMutation()
				break
			}
			var obs string
			var err error
			var res tools.ToolResult
			var job *tools.Job
			var humanAnswer *string
			switch tc.Function.Name {
			case "ask_user":
				res.Text, humanAnswer, err = c.askUser(ctx, argsJSON)
				if errors.Is(err, ErrApprovalDetached) || ctx.Err() != nil {
					if err == nil {
						err = ctx.Err()
					}
					closeMutation()
					c.LastTerminalReason = TerminalAbort
					c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
					return lastText, total, err
				}
				if humanAnswer != nil {
					reviewIntent.Answers = append(reviewIntent.Answers, json.RawMessage(*humanAnswer))
				}
			case "bash":
				if attempt != nil && attempt.foreground {
					res, err = c.Exec.Dispatch(toolCtx, "bash", argsJSON)
				} else {
					res, job, err = c.execBash(toolCtx, *tc)
				}
				if job != nil {
					pending = append(pending, pendingJob{callID: tc.ID, job: job})
				}
			default:
				res, err = c.Exec.Dispatch(toolCtx, tc.Function.Name, argsJSON)
			}
			closeMutation()
			var bashArgs tools.BashArgs
			isStop := tc.Function.Name == "bash" && json.Unmarshal([]byte(argsJSON), &bashArgs) == nil && bashArgs.KillJobID != ""
			var stopped *tools.Job
			if target := res.StoppedJob; target != nil && !target.Delivered() {
				stopped = target
				if outcome, done := target.Result(); done {
					if outcome.ExitCodeUnknown {
						res.Text = fmt.Sprintf("Stopped background job %s (exit status unavailable).", target.ID)
						res.ExitCode = nil
					} else {
						res.Text = fmt.Sprintf("Stopped background job %s (exit %d).", target.ID, outcome.ExitCode)
						res.ExitCode = &outcome.ExitCode
					}
				} else if target.Status() == tools.JobRunning {
					if !slices.ContainsFunc(pending, func(p pendingJob) bool { return p.job == target }) {
						pending = append(pending, pendingJob{callID: target.ToolCallID, job: target})
					}
					err = fmt.Errorf("stop requested for %s; completion is not confirmed yet", target.ID)
					res.Status = tools.ResultDeferred
				}
			}
			var admission *tools.BashAdmissionError
			notExecuted := errors.As(err, &admission)
			if err != nil {
				if res.Status != tools.ResultDeferred {
					res.Status = tools.ResultError
				}
				if notExecuted {
					res.Status = tools.ResultNotExecuted
				}
				obs = fmt.Sprintf("Error: %v\nOutput:\n%s", err, res.Text)
			} else {
				obs = res.Text
				if res.Status == "" {
					res.Status = tools.ResultSuccess
				}
			}
			if tc.Function.Name != tools.GuidanceTool && c.Exec != nil && c.Exec.Plugins != nil {
				obs = c.Exec.Plugins.RunToolResultHooks(ctx, tc.Function.Name, argsJSON, obs, err)
			}
			if isStop {
				// Decorations cannot block the original result's persistence.
				obs = capText(obs, 1024)
			}

			if job == nil {
				guardKey := callKey
				if guardArgs != argsJSON {
					guardKey = CanonicalToolKey(tc.Function.Name, guardArgs)
				}
				c.DoomLoop().Observe(guardKey, obs)
				if ctx.Err() == nil && c.budgetError() == nil {
					if err != nil && res.Status != tools.ResultDeferred {
						failures := c.progressFailure(callKey, failureKind(err, attempt != nil))
						if failures >= FailureWarnAfter {
							obs += "\n" + FailureNote(tc.Function.Name, failures)
						}
						recordLastError(&c.progress.lastErrors, obs)
					} else if err == nil {
						c.progressSuccess(callKey, tc.Function.Name)
					}
				}
			}

			observation := executionObservation(tc.Function.Name, argsJSON, callKey, res.Text, res.OutputFile, err, job)
			observation.Answer = humanAnswer
			endEvent := Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolCallID: tc.ID, InvocationID: inv.ID, ToolOutput: obs, ToolErr: err, ToolDetail: res.Detail, ToolCount: res.Count, ToolTruncated: res.Truncated, ToolCountLowerBound: res.CountLowerBound, ToolStatus: res.Status, ToolExitCode: res.ExitCode, ToolOutputFile: res.OutputFile}
			if !isStop {
				c.emit(endEvent)
			}
			var deliveries []jobDelivery
			if stopped != nil {
				// Persist each ready outcome before the next tool. Its facts
				// are anchored to this stop independently of provider display.
				deliveries = c.stoppedJobDeliveries()
			}
			result := llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID, InvocationID: inv.ID, ToolError: err != nil, Observation: observation}
			if notExecuted {
				result.Kind = notExecutedKind
				observation.Failure = nil
			}
			if isStop && res.Status == tools.ResultDeferred {
				result.Kind = stopPendingKind
				observation.Failure = nil
				observation.PendingStop = &llm.JobIdentity{ID: stopped.ID, Started: stopped.Started}
			}
			if res.Guidance != nil && err == nil {
				attachGuidanceDelivery(&result, *res.Guidance)
				if guidanceState != nil {
					guidanceState.unread = res.Guidance
				}
			}
			msgs = append(msgs, result)
			deliver := func(deliveries []jobDelivery) {
				for _, d := range deliveries {
					delivery := pendingStopCompletion(msgs, d.job, d.result, d.output)
					if delivery.Observation.BeforeInvocationID == "" {
						delivery.Observation.BeforeInvocationID = inv.ID
					}
					msgs = append(msgs, delivery)
					c.persist(delivery)
					if c.persistErr != nil {
						return
					}
					d.job.MarkDelivered()
					pending = slices.DeleteFunc(pending, func(p pendingJob) bool { return p.job == d.job })
					c.emit(backgroundDoneEvent(d.job, d.result, d.output, d.result.Err))
				}
			}
			deliver(deliveries)
			// A recorded stop acknowledgement must never outlive a missing
			// target outcome after a partial batch write or crash. Provider
			// replay groups tool replies first regardless of disk ordering.
			c.persist(result)
			if c.persistErr != nil {
				return lastText, total, c.persistErr
			}
			if isStop {
				c.emit(endEvent)
			}
			if isStop {
				// The target or a sibling can finish while the stop event is
				// consumed, even if the first snapshot delivered the target.
				deliver(c.stoppedJobDeliveries())
				if c.persistErr != nil {
					return lastText, total, c.persistErr
				}
			}
			for _, evidence := range c.verificationFinish(ctx, attempt, res, err) {
				msgs = append(msgs, evidence)
				c.persist(evidence)
			}
			// A read image comes back with its bytes: attach them as
			// a user message right after the tool result, so the next
			// LLM call sees the image. Tool messages cannot carry
			// images in any of the three APIs, and the image must be
			// in the history before the next call, not after it.
			if err == nil && len(res.Images) > 0 {
				imgMsg := llm.Message{Role: "user", Kind: "image", Content: llm.ImageText(res.Images), Images: res.Images}
				msgs = append(msgs, imgMsg)
				if c.Session != nil {
					c.persist(imgMsg)
				}
			}
			if budgetErr := c.budgetError(); budgetErr != nil {
				c.rejectBudgetTools(assistant.ToolCalls[i+1:], budgetErr)
				return lastText, total, budgetErr
			}
			if stopped != nil && !stopped.Delivered() {
				msgs = append(msgs, c.rejectBudgetTools(assistant.ToolCalls[i+1:], fmt.Errorf("background job %s is still finalizing; reconsider these calls after its outcome is recorded", stopped.ID))...)
				if c.persistErr != nil {
					return lastText, total, c.persistErr
				}
				break
			}
		}
		if c.progress.failures >= MaxFailedFixAttempts && ctx.Err() == nil {
			if len(pending) > 0 {
				var err error
				msgs, err = c.drainPending(ctx, msgs, pending)
				if err != nil {
					c.LastTerminalReason = TerminalAbort
					c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
					return lastText, total, err
				}
				remaining := pending[:0]
				for _, p := range pending {
					if !p.job.Delivered() {
						remaining = append(remaining, p)
					}
				}
				pending = remaining
			}
			if c.progress.failures >= MaxFailedFixAttempts {
				return finishBlocked()
			}
		}
	}
	// The step cap hit with jobs still running: deliver their results
	// before reporting the cap, so the work is not silently lost.
	// A job that is still running is left running: drainPending's
	// bound applies here too, and the job's result is delivered at
	// the next turn via the Run-start re-attach.
	if len(pending) > 0 {
		msgs, _ = c.drainPending(ctx, msgs, pending)
	}
	if c.persistErr != nil {
		return lastText, total, c.persistErr
	}
	if err := ctx.Err(); err != nil {
		c.LastTerminalReason = TerminalAbort
		c.emit(Event{Kind: EventError, Err: err, Text: lastText, Usage: total, TerminalReason: TerminalAbort})
		return lastText, total, err
	}
	// Step-cap grace period, after Gemini CLI: a hard cut throws
	// away the turn, so the model gets one final, tool-less call —
	// bounded by 60 seconds — to deliver its best answer from what
	// it has done so far. The final answer is persisted for the next
	// turn; the temporary instruction stays local. The turn still
	// ends with the cap error and TerminalMaxTurns; only the final
	// text (and its usage) is the grace answer instead of the last
	// tool-call step's text. A failed final attempt adds no assistant
	// message; a context checkpoint may already have been committed.
	if ctx.Err() == nil {
		graceText, graceUsage, ok := c.graceCall(ctx, msgs, "You have reached the step limit. Give your best final answer now, summarising what was done and what remains unfinished. Do not call any more tools.", guidanceState)
		c.addObservedUsage(&total, graceUsage)
		if ok {
			lastText = graceText
		}
	}
	capped := fmt.Errorf("step cap reached (%d); the task was not finished", steps)
	c.LastTerminalReason = TerminalMaxTurns
	c.emit(Event{Kind: EventError, Err: capped, Text: lastText, TerminalReason: TerminalMaxTurns})
	return lastText, total, capped
}

// graceCall makes the one tool-less final call after the step cap
// (or a blocked turn). It returns the model's final text, its
// usage, and whether the call succeeded. The call runs under a
// 60-second timeout. The instruction is the caller's: the cap asks
// for a best final answer, the blocked turn for a summary of what
// was verified, what failed and what is left.
func (c *Config) graceCall(ctx context.Context, msgs []llm.Message, instruction string, guidanceState *guidanceTurn) (string, llm.Usage, bool) {
	if c.persistErr != nil || c.budgetRequest() != nil {
		return "", llm.Usage{}, false
	}
	// Bound grace calls to a single attempt (no retry amplification during wrap-up).
	prevAttempts := c.MaxLLMAttempts
	c.MaxLLMAttempts = 1
	defer func() { c.MaxLLMAttempts = prevAttempts }()
	graceCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	instructionMsg := llm.Message{Role: "user", Content: instruction}
	graceMsgs := append(msgs[:len(msgs):len(msgs)], instructionMsg)
	var total llm.Usage
	if c.ContextLimit > 0 && c.contextTokens(c.promptView(graceMsgs), nil) >= c.contextBudget() {
		compacted, u, err := c.compactInLoop(graceCtx, msgs, &checkpointPreparation{guidance: guidanceState, closing: &instructionMsg})
		c.addObservedUsage(&total, u)
		c.emit(Event{Kind: EventStepUsage, Usage: u})
		if err != nil {
			return "", total, false
		}
		graceMsgs = append(compacted, instructionMsg)
		if !c.checkpointContextFits(graceMsgs, nil) {
			return "", total, false
		}
	}
	apiMsgs := c.promptView(graceMsgs)
	result, err := c.streamVisibleStep(graceCtx, apiMsgs, nil)
	final, usage, spent := result.assistant, result.usage, result.spent
	c.addObservedUsage(&total, spent)
	if usage.PromptTokens > 0 {
		c.LastPromptTokens = usage.PromptTokens
		c.lastPromptEstimate = estimatePromptTokens(apiMsgs, nil)
	}
	c.emit(Event{Kind: EventStepUsage, Usage: spent})
	if err != nil || final.Content == "" || len(final.ToolCalls) > 0 || final.FinishReason == "length" || final.FinishReason == "max_tokens" || final.FinishReason == "incomplete" {
		return "", total, false
	}
	if c.Session != nil {
		c.persist(final)
		if c.persistErr != nil {
			return "", total, false
		}
	}
	return final.Content, total, true
}

// stoppedReport gives callers a usable failure report even if the last tool-call
// response contained only whitespace. It makes no claims about work verification.
func stoppedReport(lastText string, err error) string {
	if strings.TrimSpace(lastText) != "" {
		return lastText
	}
	return fmt.Sprintf("The turn stopped before a final response: %v.\nNo final verification report was produced; completed actions have not been rolled back. Check the current state before resuming.", err)
}

// blockedGracePrompt builds the blocked turn's grace instruction
// from the blocked error and the last failed observations, so the
// summary names what actually failed instead of guessing.
func blockedGracePrompt(blocked error, lastErrors []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The task was blocked: %v. ", blocked)
	if len(lastErrors) > 0 {
		b.WriteString("The last errors were:\n")
		for _, e := range lastErrors {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	b.WriteString("Give a short summary: what was verified or completed, what failed and why, and what is left to do. Do not call any more tools.")
	return b.String()
}

// recordLastError keeps the last few failed observations for the
// blocked-grace summary, newest last, each capped so the prompt
// stays small.
func recordLastError(errs *[]string, obs string) {
	*errs = append(*errs, truncate(obs, 500))
	if len(*errs) > 3 {
		*errs = (*errs)[len(*errs)-3:]
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return textutil.Prefix(s, n) + "…"
	}
	return s
}

// ResetSessionState clears guards and token accounting owned by one session.
func (c *Config) ResetSessionState() {
	c.doomLoop = nil
	c.LastPromptTokens = 0
	c.lastPromptEstimate = 0
	c.LastTerminalReason = ""
	c.LastTaskOutcome = ""
	c.verifier = nil
	c.LastTurnBudget = nil
}

// persist latches a storage failure. The loop stops before its next action;
// continuing without a durable tool-call record would make recovery unsafe.
func (c *Config) persist(m llm.Message) {
	if c.Session != nil && c.persistErr == nil {
		if err := c.Session.Append(m); err != nil {
			c.persistErr = fmt.Errorf("save session: %w", err)
		}
	}
}
