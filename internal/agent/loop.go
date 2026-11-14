// Package agent runs the single loop: LLM -> tool calls -> observe -> repeat.
// Approval is a commitment/UX boundary, not a sandbox: with bash available,
// denying write/edit can be bypassed. Run on disposable copies or in a container.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

type Config struct {
	Client   *llm.Client
	Exec     *tools.Executor
	Tools    []llm.ToolDef
	System   string
	Mode     string // "suggest" (ask) or "auto" (no ask)
	MaxSteps int
	Session  *session.Store
	// OnEvent receives every loop event; the caller renders them.
	// (There is no OnText: streamed text arrives as TextDelta events.)
	OnEvent func(Event)
	Ask     func(prompt string) (bool, error) // nil = use stdin prompt
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
	// DenyInsteadOfPrompt refuses every approval without asking (copy of
	// Claude Code's permissions.denyInsteadOfPrompting). -p print mode
	// sets it: an unattended run must never block waiting for a human.
	// It only ever denies, never approves.
	DenyInsteadOfPrompt bool

	// LastPromptTokens is the prompt-token count of the most recent LLM
	// call in the last Run: the current context size, since history is
	// resent in full each time. Used for the compaction trigger.
	LastPromptTokens int
	// LastTerminalReason is why the most recent Run ended, in the
	// structured form the RPC server reports as agent_settled.reason.
	LastTerminalReason TerminalReason

	// doomLoop is the per-Config guard: its streak and session
	// allowlist outlive one turn, like Qwen's agent-context guard.
	doomLoop *DoomLoopGuard
}

// DoomLoop returns the Config's guard, creating it on first use.
func (c *Config) DoomLoop() *DoomLoopGuard {
	if c.doomLoop == nil {
		c.doomLoop = NewDoomLoopGuard(c.DoomLoopThreshold)
	}
	return c.doomLoop
}

func (c *Config) emit(e Event) {
	if c.OnEvent != nil {
		c.OnEvent(e)
	}
}

func ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		llm.Tool("read", "Read a file (with offset/limit) or list a directory. Image files (png, jpg, gif, webp) are returned as an image you can see, not as text — use read to look at a screenshot or photo.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}`),
		llm.Tool("write", "Create a file. By default it fails if the file already exists — use edit then. Pass overwrite:true to replace an existing file wholesale; prefer edit for a small change.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"overwrite":{"type":"boolean"}},"required":["path","content"]}`),
		llm.Tool("edit", "Replace text in an existing file. Two forms: oldText (replaced exactly once) or startLine/endLine (1-based, inclusive; endLine defaults to startLine) to replace those lines with newText, ignoring oldText. After a read, prefer the line form: line numbers cannot go stale the way oldText can.", `{"type":"object","properties":{"path":{"type":"string"},"oldText":{"type":"string"},"newText":{"type":"string"},"startLine":{"type":"integer"},"endLine":{"type":"integer"}},"required":["path","newText"]}`),
		llm.Tool("bash", "Run a shell command in the workspace root. Each call is a fresh shell; cd does not persist between calls — pass cwd to run in a subdirectory, or prefix the command with `cd <dir> && `. Timeout in seconds, default 120, max 600; a background:true job has no timeout unless one is passed. Pass background:true for a command that will not exit on its own (dev server, watcher): it returns at once with a job id, and its output is delivered at the next step. A never-ending job never blocks the turn from ending: the turn ends after a short drain wait, and you can read its partial output from the output file. Every result header names the directory the call ran in.", `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer"},"background":{"type":"boolean"},"cwd":{"type":"string"}},"required":["command"]}`),
		llm.Tool("grep", "Search file contents with a regex. Prefer this over bash grep/rg. Returns file:line matches, capped (truncation is marked).", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"include":{"type":"string"},"exclude":{"type":"string"},"context":{"type":"integer"},"maxMatches":{"type":"integer"}},"required":["pattern"]}`),
		llm.Tool("glob", "List files matching a glob pattern, e.g. **/*.go. Prefer this over bash find/ls for discovering files.", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"limit":{"type":"integer"}},"required":["pattern"]}`),
		llm.Tool("load_skill", "Load the full text of an available skill by name when its guidance is needed for the current task. The skill names and one-line descriptions are in the system prompt. Pass reference (a file name from the skill's references/ footer) to read one reference file instead of the whole skill.", `{"type":"object","properties":{"name":{"type":"string"},"reference":{"type":"string"}},"required":["name"]}`),
		llm.Tool("webfetch", "Fetch a URL and return its content as Markdown (HTML is converted to Markdown, keeping headings, lists, links, code and tables; scripts and styles are dropped). Use this to read web pages, docs and API responses instead of curl. Returns up to 30000 characters per call; use offset to page through a long page. format=text returns plain text instead of Markdown; timeout is seconds (default 30, max 120). Private, loopback (except localhost) and cloud-metadata addresses are refused, and cross-host redirects are not followed.", `{"type":"object","properties":{"url":{"type":"string"},"format":{"type":"string","enum":["markdown","text"]},"offset":{"type":"integer"},"timeout":{"type":"integer"}},"required":["url"]}`),
		llm.Tool("ask_user", "Ask the user structured questions when a decision needs their input instead of guessing: 1-4 questions, each with 2-4 options (plus free-text Other). Use it in plan mode to clarify requirements, and anywhere a choice changes what you will do. The user picks options or types their own answer; their answers come back as this call's result.", `{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{"question":{"type":"string"},"header":{"type":"string"},"multiSelect":{"type":"boolean"},"options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"label":{"type":"string"},"description":{"type":"string"}},"required":["label"]}}},"required":["question","options"]}}},"required":["questions"]}`),
	}
}

func (c *Config) ask(prompt string) (bool, error) {
	if c.DenyInsteadOfPrompt {
		return false, nil
	}
	if c.Ask != nil {
		return c.Ask(prompt)
	}
	fmt.Printf("%s [y/N] ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// checkRepeatedCall runs the doom-loop guard for one requested
// call. It returns the arguments to execute (possibly the user's
// edited version) and whether the call may run. A denied call must
// end the turn: the caller reports it with
// TerminalRepeatedToolCallDenied.
func (c *Config) checkRepeatedCall(ctx context.Context, toolName, argsJSON string) (string, bool) {
	guard := c.DoomLoop()
	detected, det := guard.Check(toolName, argsJSON)
	if !detected {
		return argsJSON, true
	}
	// Headless (no one to ask, or approvals denied without asking):
	// deny. A headless run must never assume the user would continue.
	if c.ConfirmContinuation == nil || c.DenyInsteadOfPrompt || ctx.Err() != nil {
		return argsJSON, false
	}
	dec, err := c.ConfirmContinuation(ContinuationRequest{
		ToolName: det.ToolName, RepeatCount: det.RepeatCount,
		Threshold: det.Threshold, InputPreview: det.InputPreview,
	})
	if err != nil {
		return argsJSON, false
	}
	key := det.InputKey
	useArgs := argsJSON
	if dec.UpdatedArgs != "" {
		key = CanonicalToolKey(toolName, dec.UpdatedArgs)
		useArgs = dec.UpdatedArgs
	}
	switch dec.Outcome {
	case ContinuationSession:
		guard.AllowSession(key)
		return useArgs, true
	case ContinuationOnce:
		guard.AllowOnce(key)
		return useArgs, true
	default:
		return argsJSON, false
	}
}

// askUser runs one ask_user tool call: validate the questions,
// ask them through the AskQuestion seam, and render the answers
// as the observation. Invalid arguments and a cancelled question
// are observations, not turn errors, so the model can recover.
func (c *Config) askUser(ctx context.Context, argsJSON string) string {
	req, err := ParseAskUserArgs(argsJSON)
	if err != nil {
		return fmt.Sprintf("Error: %v\nAsk at most %d questions, each with 2-%d options, and try again.", err, MaxQuestionsPerAsk, MaxOptionsPerQuestion)
	}
	if c.AskQuestion == nil || ctx.Err() != nil {
		return "No one is available to answer questions right now. Proceed with your best judgement and state the assumptions you made."
	}
	ans, err := c.AskQuestion(req)
	if err != nil {
		return "The questions were cancelled. Proceed with your best judgement and state the assumptions you made."
	}
	return FormatAnswers(req, ans)
}

func (c *Config) needsApproval(name string) bool {
	if c.Mode == "auto" {
		return false
	}
	return name == "write" || name == "edit" || name == "bash"
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
func (c *Config) execBash(ctx context.Context, tc llm.ToolCall) (string, *tools.Job) {
	threshold := time.Duration(0)
	if c.Exec != nil {
		threshold = c.Exec.BackgroundThreshold
	}
	var a tools.BashArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
		return fmt.Sprintf("Error: bad bash args: %v\nOutput:\n", err), nil
	}
	// backgrounded returns the observation for a job that keeps
	// running, and the job itself so Run can feed its result back
	// at the next step boundary.
	backgrounded := func(job *tools.Job) (string, *tools.Job) {
		c.emit(Event{Kind: EventToolBackgrounded, ToolName: "bash", ToolArgs: tc.Function.Arguments, JobID: job.ID})
		msg := fmt.Sprintf("Command is still running in the background (job %s). Continue with other work; its output will be delivered automatically when it finishes. Do not re-run the command, and do not wait for it with sleep or polling.", job.ID)
		if job.OutputFile != "" {
			msg += fmt.Sprintf(" You can read its partial output so far with read %q.", job.OutputFile)
		}
		return msg, job
	}
	if a.Background {
		// Explicit background, chosen by the model: start and
		// return at once, without waiting out the threshold.
		// For servers and watchers that never exit on their own.
		job, err := c.Exec.BashBackground(ctx, a)
		if err != nil {
			// Start failure (denylist): an observation, never a job.
			return fmt.Sprintf("Error: %v\nOutput:\n", err), nil
		}
		return backgrounded(job)
	}
	if threshold <= 0 {
		out, err := c.Exec.Bash(ctx, a)
		if err != nil {
			return fmt.Sprintf("Error: %v\nOutput:\n%s", err, out), nil
		}
		return out, nil
	}
	job, err := c.Exec.BashForeground(ctx, a)
	if err != nil {
		// Start failure (denylist): an observation, never a job.
		return fmt.Sprintf("Error: %v\nOutput:\n", err), nil
	}
	// deliver converts a finished job's result into an observation.
	// It also drops the job from /jobs: it never backgrounded.
	deliver := func() (string, *tools.Job) {
		res, _ := job.Result()
		c.Exec.ForgetJob(job)
		if res.Err != nil {
			return fmt.Sprintf("Error: %v\nOutput:\n%s", res.Err, res.Output), nil
		}
		return res.Output, nil
	}
	select {
	case <-ctx.Done():
		job.Cancel()
		<-job.Done()
		return deliver()
	case <-job.Done():
		return deliver()
	case <-time.After(threshold):
		// It may have finished just as the threshold fired: deliver
		// it normally instead of backgrounding a finished job.
		if _, ok := job.Result(); ok {
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
		return fmt.Sprintf("Background job %s finished with error: %v (exit %d).\nOutput:\n%s", job.ID, res.Err, res.ExitCode, capText(res.Output, 32*1024)), res.Err
	}
	return fmt.Sprintf("Background job %s finished (exit %d).\nOutput:\n%s", job.ID, res.ExitCode, capText(res.Output, 32*1024)), nil
}

// collectFinishedJobs appends a user message for each pending job
// that finished since the last step, in completion order, and drops
// them from pending. It never blocks: a still-running job stays
// pending for a later step or the turn-end drain.
func (c *Config) collectFinishedJobs(msgs []llm.Message, pending []pendingJob) ([]llm.Message, []pendingJob) {
	var done, rest []pendingJob
	for _, p := range pending {
		if _, ok := p.job.Result(); ok {
			done = append(done, p)
		} else {
			rest = append(rest, p)
		}
	}
	if len(done) == 0 {
		return msgs, pending
	}
	sort.Slice(done, func(i, j int) bool {
		ti, _ := done[i].job.FinishedAt()
		tj, _ := done[j].job.FinishedAt()
		return ti.Before(tj)
	})
	for _, p := range done {
		res, _ := p.job.Result()
		obs, toolErr := jobResultMessage(p.job, res)
		c.emit(Event{Kind: EventToolBackgroundDone, ToolName: "bash", ToolOutput: obs, ToolErr: toolErr, JobID: p.job.ID})
		p.job.MarkDelivered()
		msgs = append(msgs, llm.Message{Role: "user", Content: obs})
		if c.Session != nil {
			_ = c.Session.Append(msgs[len(msgs)-1])
		}
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
	var doneJobs, freshJobs, oldJobs []*tools.Job
	for _, job := range jobs {
		if _, ok := job.Result(); ok {
			doneJobs = append(doneJobs, job)
		} else if job.Reported() {
			oldJobs = append(oldJobs, job)
		} else {
			freshJobs = append(freshJobs, job)
		}
	}
	sort.Slice(doneJobs, func(i, j int) bool {
		ti, _ := doneJobs[i].FinishedAt()
		tj, _ := doneJobs[j].FinishedAt()
		return ti.Before(tj)
	})
	for _, job := range doneJobs {
		res, _ := job.Result()
		obs, toolErr := jobResultMessage(job, res)
		c.emit(Event{Kind: EventToolBackgroundDone, ToolName: "bash", ToolOutput: obs, ToolErr: toolErr, JobID: job.ID})
		job.MarkDelivered()
		msgs = append(msgs, llm.Message{Role: "user", Content: obs})
		if c.Session != nil {
			_ = c.Session.Append(msgs[len(msgs)-1])
		}
	}
	if len(freshJobs) == 0 {
		// Nothing new to wait for: the only unfinished jobs are
		// ones the model was already told about.
		return msgs, nil
	}
	c.emit(Event{Kind: EventBackgroundWait, Text: strings.Join(jobIDs, ", ")})
	// Deliver in completion order, not submission order: a slow
	// job must not hold back results that are already ready. One
	// goroutine per job signals a shared channel when its job
	// finishes.
	done := make(chan *tools.Job, len(freshJobs))
	for _, job := range freshJobs {
		go func(j *tools.Job) { <-j.Done(); done <- j }(job)
	}
	bound := c.drainTimeout()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	remaining := len(freshJobs)
	for remaining > 0 {
		select {
		case job := <-done:
			res, _ := job.Result()
			obs, toolErr := jobResultMessage(job, res)
			c.emit(Event{Kind: EventToolBackgroundDone, ToolName: "bash", ToolOutput: obs, ToolErr: toolErr, JobID: job.ID})
			job.MarkDelivered()
			msgs = append(msgs, llm.Message{Role: "user", Content: obs})
			if c.Session != nil {
				_ = c.Session.Append(msgs[len(msgs)-1])
			}
			remaining--
		case <-timer.C:
			// Bound reached: end the turn with the jobs still
			// running instead of waiting forever. Report each
			// one with its output file path so the model can
			// read partial output; mark them reported so the
			// next turn's drain does not wait on them again.
			var still []*tools.Job
			for _, job := range freshJobs {
				if _, ok := job.Result(); !ok {
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
				msgs = append(msgs, llm.Message{Role: "user", Content: b.String()})
				if c.Session != nil {
					_ = c.Session.Append(msgs[len(msgs)-1])
				}
			}
			return msgs, nil
		case <-ctx.Done():
			return msgs, ctx.Err()
		}
	}
	return msgs, nil
}

// MaxStopBlocks caps consecutive turn-end vetoes by stop hooks,
// after Claude Code's Stop hook (8): after that many blocks the
// turn is allowed to end anyway, so a broken check cannot trap the
// model forever.
const MaxStopBlocks = 8

// MaxFailedFixAttempts is the consecutive all-failed tool steps
// after which the task is marked blocked instead of looping: the
// model tried (differently — identical repeats are the doom-loop
// guard's job) and every attempt failed.
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
func (c *Config) streamStep(ctx context.Context, apiMsgs []llm.Message, tools []llm.ToolDef, onText func(string), onDone func(llm.Message, llm.Usage), onThinking func(string), onRetry func()) (int, error) {
	attempts := c.maxLLMAttempts()
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		outputSeen := false
		doneSeen := false
		wrappedText := func(s string) {
			outputSeen = true
			if onText != nil {
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
			outputSeen = true
			if onThinking != nil {
				onThinking(s)
			}
		}
		err = c.Client.Stream(ctx, apiMsgs, tools, wrappedText, wrappedDone, wrappedThinking)
		if err == nil {
			return attempt, nil
		}
		if doneSeen || ctx.Err() != nil || !llm.IsRetryable(err) || attempt == attempts {
			return attempt, err
		}
		if outputSeen {
			// The stream was cut off after showing partial output:
			// discard the partial reply before replaying. The
			// assistant message and usage need no reset here: they
			// are only written by onDone, which sets doneSeen and
			// blocks the retry above. Only the caller's thinking
			// accumulator holds per-attempt state, and onRetry
			// resets it.
			c.emit(Event{Kind: EventStepDiscard, Text: fmt.Sprintf("LLM stream cut off after partial output (%v); discarding the partial reply", err), RetryAttempt: attempt + 1})
			if onRetry != nil {
				onRetry()
			}
		}
		delay := retryDelay(attempt, llm.RetryAfter(err))
		c.emit(Event{Kind: EventStepRetry, Text: fmt.Sprintf("LLM call failed (attempt %d/%d): %v; retrying in %s", attempt, attempts, err, delay), RetryAttempt: attempt + 1})
		if !sleepCtx(ctx, delay) {
			return attempt, err
		}
	}
	return attempts, err
}

// Run executes one user turn. It appends messages to the session store
// and returns the assistant's final text, total tokens used, and error.
func (c *Config) Run(ctx context.Context, history []llm.Message) (string, llm.Usage, error) {
	if c.Exec != nil {
		stop := context.AfterFunc(ctx, c.Exec.SessionCanceller())
		defer stop()
	}
	steps := c.MaxSteps
	// MaxSteps <= 0 means unlimited (the mainstream default): the turn
	// runs until the model stops calling tools, an error ends it, or
	// the task is blocked. The bounds are then auto-compaction at 70%
	// of the context window plus the doom-loop / blocked guards. A
	// positive MaxSteps opts into a hard cap with the grace summary.
	hasCap := steps > 0
	msgs := append([]llm.Message{{Role: "system", Content: c.System}}, history...)
	var total llm.Usage
	var lastText string
	// pending background bash jobs, in call order. Their results are
	// fed back as user messages when the turn would otherwise end
	// (see drainPending).
	var pending []pendingJob
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
			}
		}
	}
	// stopBlocks counts consecutive turn-end vetoes in this turn;
	// failedFixes counts consecutive steps whose tool calls all
	// failed (a step with any success resets it).
	stopBlocks := 0
	failedFixes := 0
	// breaker counts failed tool calls per fingerprint within
	// this turn (a new breaker per Run: counts never leak into
	// the next turn). lastErrors keeps the last few failed
	// observations for the blocked-grace summary.
	breaker := NewFailureBreaker()
	var lastErrors []string

	for step := 0; !hasCap || step < steps; step++ {
		// Finished background jobs report in at the next step,
		// in completion order — the model sees results as soon
		// as they are ready, not only when the turn would end.
		msgs, pending = c.collectFinishedJobs(msgs, pending)
		var assistant llm.Message
		var usage llm.Usage
		// thinking accumulates this step's thinking text. It is
		// display-only: it never enters the assistant message or
		// the session, and thinking_end fires only when it is
		// non-empty.
		var thinking strings.Builder
		// Send the truncated view to the LLM; the full history stays
		// in msgs and in the session file. The cap is uniform (see
		// truncate.go): a result's sent form never changes after
		// its first send, so the cached prefix stays valid. Old tool
		// results are then pruned to a fixed placeholder once the
		// history passes the prune threshold, which bounds prompt
		// growth on long runs; the prune's block boundaries are
		// frozen, so the sent view stays append-only between prunes.
		apiMsgs := truncateToolResults(msgs, toolResultMaxChars)
		apiMsgs = pruneOldToolResults(apiMsgs, pruneThresholdChars, pruneBlockChars, pruneKeepNewestBlocks)
		attempts, err := c.streamStep(ctx, apiMsgs, c.Tools, func(s string) {
			c.emit(Event{Kind: EventTextDelta, Text: s})
		}, func(m llm.Message, u llm.Usage) {
			assistant = m
			usage = u
			if thinking.Len() > 0 {
				c.emit(Event{Kind: EventThinkingEnd, Text: thinking.String()})
			}
		}, func(s string) {
			thinking.WriteString(s)
			c.emit(Event{Kind: EventThinkingDelta, Text: s})
		}, func() {
			// A discarded attempt's thinking must not leak into
			// the replay's ThinkingEnd: the builder holds only
			// this step's thinking, and the discard drops it.
			thinking.Reset()
		})
		if err != nil {
			wrapped := fmt.Errorf("step %d: %w", step+1, err)
			if attempts > 1 {
				wrapped = fmt.Errorf("step %d: %w (after %d attempts)", step+1, err, attempts)
			}
			// The turn is over, but a running job's result is still
			// the user's data: deliver it before reporting the error
			// so it is not silently lost.
			if len(pending) > 0 {
				_, _ = c.drainPending(ctx, msgs, pending)
			}
			// A cancelled context is an abort, not a provider error:
			// the RPC server reports it as reason "abort" with no
			// error string (see TerminalAbort).
			c.LastTerminalReason = TerminalError
			if ctx.Err() != nil {
				c.LastTerminalReason = TerminalAbort
			}
			c.emit(Event{Kind: EventError, Err: wrapped, Text: lastText, TerminalReason: c.LastTerminalReason})
			return lastText, total, wrapped
		}
		c.LastPromptTokens = usage.PromptTokens
		total.Add(usage)
		c.emit(Event{Kind: EventStepUsage, Usage: usage})
		lastText = assistant.Content
		msgs = append(msgs, assistant)
		if c.Session != nil {
			_ = c.Session.Append(assistant)
		}
		if len(assistant.ToolCalls) == 0 {
			if len(pending) == 0 {
				// Turn-end gate (Claude's Stop hook): a plugin stop
				// hook may veto the end — e.g. a check the final
				// answer failed. The reason is fed back to the
				// model as a user message and the turn continues,
				// up to MaxStopBlocks vetoes; after that the turn
				// is allowed to end anyway.
				if ctx.Err() == nil && c.Exec != nil && c.Exec.Plugins != nil && stopBlocks < MaxStopBlocks {
					sessionID := ""
					if c.Session != nil {
						sessionID = c.Session.ID
					}
					if reason := c.Exec.Plugins.RunStopHooks(ctx, sessionID, lastText); reason != "" {
						stopBlocks++
						c.emit(Event{Kind: EventStopBlocked, Text: reason})
						blockMsg := llm.Message{Role: "user", Content: "A stop hook blocked the turn from ending: " + reason + " Address it before finishing, then answer again."}
						msgs = append(msgs, blockMsg)
						if c.Session != nil {
							_ = c.Session.Append(blockMsg)
						}
						continue
					}
				}
				c.LastTerminalReason = TerminalEndTurn
				c.emit(Event{Kind: EventDone, Text: lastText, Usage: total, TerminalReason: TerminalEndTurn})
				return lastText, total, nil
			}
			// The model stopped calling tools but background jobs are
			// still running: wait for them, feed the results back,
			// and let the model act on them in the next LLM call.
			var err error
			msgs, err = c.drainPending(ctx, msgs, pending)
			pending = nil
			if err != nil {
				return lastText, total, err
			}
			continue
		}
		// Execute tool calls in order. (Parallel batches are v0.2; serial
		// avoids two tools writing the same file at once.)
		// stepHadExecution / stepAllFailed track this step for the
		// blocked-after-3-failures rule: only calls that actually
		// executed count (denials and refusals are not fix attempts).
		stepHadExecution := false
		stepAllFailed := true
		for _, tc := range assistant.ToolCalls {
			if err := ctx.Err(); err != nil {
				c.LastTerminalReason = TerminalAbort
				c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: TerminalAbort})
				return lastText, total, err
			}
			c.emit(Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
			// Plugin tool_call hooks run before approval: a denied
			// call never prompts the user, and the approval flow
			// still applies to anything the hooks let through — a
			// hook can add a gate, never remove one. A rewrite
			// replaces the args the rest of this path sees
			// (approval prompt, audit, dispatch); the assistant
			// message already in history keeps what the model asked.
			argsJSON := tc.Function.Arguments
			if c.Exec != nil && c.Exec.Plugins != nil {
				newArgs, deny := c.Exec.Plugins.RunToolCallHooks(ctx, tc.Function.Name, argsJSON)
				if deny != "" {
					obs := "Denied by plugin " + deny + ". Do not retry this tool call; explain and propose an alternative."
					if c.Session != nil {
						_ = c.Session.Audit(tools.AuditEntry{TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: deny})
					}
					c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs, ToolErr: fmt.Errorf("denied by plugin %s", deny)})
					msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
					if c.Session != nil {
						_ = c.Session.Append(msgs[len(msgs)-1])
					}
					continue
				}
				argsJSON = newArgs
				tc.Function.Arguments = argsJSON
			}
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
				obs := fmt.Sprintf("Tool %s is not available in this turn. Do not retry this tool call; explain and propose an alternative.", tc.Function.Name)
				if c.Session != nil {
					_ = c.Session.Audit(tools.AuditEntry{TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: "tool not offered in this turn"})
				}
				c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs, ToolErr: fmt.Errorf("tool %s not offered in this turn", tc.Function.Name)})
				msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
				if c.Session != nil {
					_ = c.Session.Append(msgs[len(msgs)-1])
				}
				continue
			}
			// Doom-loop guard: an identical call for the fifth
			// consecutive time asks the user (once / session /
			// deny), before any approval prompt — the user must
			// not be asked twice about the same call. A denied
			// call ends the turn with the structured reason.
			useArgs, allowed := c.checkRepeatedCall(ctx, tc.Function.Name, argsJSON)
			if !allowed {
				obs := RepeatedToolCallMessage(tc.Function.Name, RepeatedToolCallThreshold)
				if c.Session != nil {
					_ = c.Session.Audit(tools.AuditEntry{TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: "repeated tool call denied"})
				}
				c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs})
				msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
				if c.Session != nil {
					_ = c.Session.Append(msgs[len(msgs)-1])
				}
				if len(pending) > 0 {
					_, _ = c.drainPending(ctx, msgs, pending)
				}
				c.LastTerminalReason = TerminalRepeatedToolCallDenied
				denied := fmt.Errorf("%w: %s repeated %d times with identical input", ErrRepeatedToolCallDenied, tc.Function.Name, RepeatedToolCallThreshold)
				c.emit(Event{Kind: EventError, Err: denied, Text: lastText, TerminalReason: TerminalRepeatedToolCallDenied})
				return lastText, total, denied
			}
			argsJSON = useArgs
			tc.Function.Arguments = argsJSON
			callKey := CanonicalToolKey(tc.Function.Name, argsJSON)
			// Repeat-failure circuit breaker: a call that already
			// failed FailureDenyAfter times this turn is denied
			// before it runs — repeating it cannot make progress,
			// and the turn must not burn steps on it. Unlike the
			// doom-loop denial, this does not end the turn: the
			// model gets the denial as a tool error and can try
			// something else. It counts as a failed fix attempt
			// for the blocked guard (the model did try to fix
			// something; the harness refused on evidence of prior
			// failure), unlike a hook/approval denial, which never
			// executed. Checked before approval, so the user is
			// never asked about a call that will be denied.
			if denied, failures := breaker.Check(callKey); denied {
				obs := RepeatedFailureDeniedMessage(tc.Function.Name, failures)
				if c.Session != nil {
					_ = c.Session.Audit(tools.AuditEntry{TS: time.Now(), Kind: "denied", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), Decision: "denied", ExitCode: -1, Error: "repeated tool failure denied"})
				}
				c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs, ToolErr: ErrRepeatedToolFailureDenied})
				msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
				if c.Session != nil {
					_ = c.Session.Append(msgs[len(msgs)-1])
				}
				stepHadExecution = true
				recordLastError(&lastErrors, obs)
				continue
			}
			if c.needsApproval(tc.Function.Name) {
				prompt := fmt.Sprintf("Run %s %s?", tc.Function.Name, truncate(tc.Function.Arguments, 200))
				c.emit(Event{Kind: EventApproval, Text: prompt})
				ok, err := c.ask(prompt)
				// Audit every decision (approved, denied, or errored)
				// to the session sidecar, for later review.
				if c.Session != nil {
					entry := tools.AuditEntry{TS: time.Now(), Kind: "approval", Tool: tc.Function.Name, Args: truncate(tc.Function.Arguments, 500), ExitCode: -1}
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
					c.LastTerminalReason = TerminalError
					if ctx.Err() != nil {
						c.LastTerminalReason = TerminalAbort
					}
					c.emit(Event{Kind: EventError, Err: err, Text: lastText, TerminalReason: c.LastTerminalReason})
					return lastText, total, err
				}
				if !ok {
					obs := "User denied this tool call. Do not retry it; explain and propose an alternative."
					c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs})
					msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
					if c.Session != nil {
						_ = c.Session.Append(msgs[len(msgs)-1])
					}
					continue
				}
			}
			var obs string
			var err error
			var res tools.ToolResult
			var job *tools.Job
			if tc.Function.Name == "ask_user" {
				// Structured questions are intercepted here, not
				// dispatched: there is no executor tool of this
				// name, and asking never needs approval.
				obs = c.askUser(ctx, argsJSON)
			} else if tc.Function.Name == "bash" {
				obs, job = c.execBash(ctx, tc)
				if job != nil {
					pending = append(pending, pendingJob{callID: tc.ID, job: job})
				}
			} else {
				var dispatchErr error
				res, dispatchErr = c.Exec.Dispatch(ctx, tc.Function.Name, argsJSON)
				obs = res.Text
				err = dispatchErr
				if err != nil {
					// Tool errors are observations, not fatal errors.
					obs = fmt.Sprintf("Error: %v\nOutput:\n%s", err, res.Text)
				}
				// Plugin tool_result hooks rewrite the observation
				// before it enters history and the session: the
				// model sees the rewritten text, and the session
				// records it too. Bash observations never reach a
				// hook (they do not go through Dispatch).
				if c.Exec.Plugins != nil {
					obs = c.Exec.Plugins.RunToolResultHooks(ctx, tc.Function.Name, argsJSON, obs, err)
				}
			}
			// A finished (not backgrounded) call whose observation
			// differs from the previous one for the same call is
			// making progress: the doom-loop streak clears. A
			// backgrounded call has no result yet, so neither
			// guard sees it here — its result surfaces later as a
			// user message, not a tool result.
			if job == nil {
				c.DoomLoop().Observe(callKey, obs)
				stepHadExecution = true
				failed := err != nil || strings.HasPrefix(obs, "Error:")
				if failed {
					failures := breaker.ObserveFailure(callKey)
					if failures >= FailureWarnAfter {
						obs += "\n" + FailureNote(tc.Function.Name, failures)
					}
					recordLastError(&lastErrors, obs)
				} else {
					stepAllFailed = false
					breaker.ObserveSuccess(callKey)
				}
			}
			c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs, ToolErr: err, ToolDetail: res.Detail, ToolCount: res.Count})
			msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
			if c.Session != nil {
				_ = c.Session.Append(msgs[len(msgs)-1])
			}
			// A read image comes back with its bytes: attach them as
			// a user message right after the tool result, so the next
			// LLM call sees the image. Tool messages cannot carry
			// images in any of the three APIs, and the image must be
			// in the history before the next call, not after it.
			if err == nil && len(res.Images) > 0 {
				imgMsg := llm.Message{Role: "user", Content: llm.ImageText(res.Images), Images: res.Images}
				msgs = append(msgs, imgMsg)
				if c.Session != nil {
					_ = c.Session.Append(imgMsg)
				}
			}
		}
		// Three consecutive steps whose tool calls all failed mark
		// the task blocked: the model is trying and failing, and
		// looping further only burns context. Any success resets
		// the count.
		if stepHadExecution {
			if stepAllFailed {
				failedFixes++
			} else {
				failedFixes = 0
			}
			if failedFixes >= MaxFailedFixAttempts {
				if len(pending) > 0 {
					_, _ = c.drainPending(ctx, msgs, pending)
				}
				blocked := fmt.Errorf("task blocked: %d consecutive steps ended with only failed tool calls; the last error was not fixed by retrying", MaxFailedFixAttempts)
				// Blocked-grace summary, mirroring the step-cap
				// grace: a turn that ends blocked otherwise
				// settles with whatever text the last tool-call
				// step had — usually nothing — so the GUI gets
				// no report at all (the bookstore run's verify
				// turn settled blocked with an empty text). One
				// tool-less final call asks for a short summary
				// of what was verified, what failed and what is
				// left; it is not persisted, and a failed grace
				// call changes nothing.
				if ctx.Err() == nil {
					if graceText, graceUsage, ok := c.graceCall(ctx, msgs, blockedGracePrompt(blocked, lastErrors)); ok {
						lastText = graceText
						total.Add(graceUsage)
					}
				}
				c.LastTerminalReason = TerminalBlocked
				c.emit(Event{Kind: EventError, Err: blocked, Text: lastText, TerminalReason: TerminalBlocked})
				return lastText, total, blocked
			}
		}
	}
	// The step cap hit with jobs still running: deliver their results
	// before reporting the cap, so the work is not silently lost.
	// A job that is still running is left running: drainPending's
	// bound applies here too, and the job's result is delivered at
	// the next turn via the Run-start re-attach.
	if len(pending) > 0 {
		_, _ = c.drainPending(ctx, msgs, pending)
	}
	// Step-cap grace period, after Gemini CLI: a hard cut throws
	// away the turn, so the model gets one final, tool-less call —
	// bounded by 60 seconds — to deliver its best answer from what
	// it has done so far. The grace call is not persisted: the
	// instruction and its reply live only in this turn's local
	// history, so the next turn does not see them. The turn still
	// ends with the cap error and TerminalMaxTurns; only the final
	// text (and its usage) is the grace answer instead of the last
	// tool-call step's text. A failed grace call changes nothing.
	if ctx.Err() == nil {
		if graceText, graceUsage, ok := c.graceCall(ctx, msgs, "You have reached the step limit. Give your best final answer now, summarising what was done and what remains unfinished. Do not call any more tools."); ok {
			lastText = graceText
			total.Add(graceUsage)
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
func (c *Config) graceCall(ctx context.Context, msgs []llm.Message, instruction string) (string, llm.Usage, bool) {
	graceCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	graceMsgs := append(append([]llm.Message{}, msgs...),
		llm.Message{Role: "user", Content: instruction})
	apiMsgs := truncateToolResults(graceMsgs, toolResultMaxChars)
	apiMsgs = pruneOldToolResults(apiMsgs, pruneThresholdChars, pruneBlockChars, pruneKeepNewestBlocks)
	var final llm.Message
	var usage llm.Usage
	var thinking strings.Builder
	_, err := c.streamStep(graceCtx, apiMsgs, nil, func(s string) {
		c.emit(Event{Kind: EventTextDelta, Text: s})
	}, func(m llm.Message, u llm.Usage) {
		final = m
		usage = u
		if thinking.Len() > 0 {
			c.emit(Event{Kind: EventThinkingEnd, Text: thinking.String()})
		}
	}, func(s string) {
		thinking.WriteString(s)
		c.emit(Event{Kind: EventThinkingDelta, Text: s})
	}, func() {
		thinking.Reset()
	})
	if err != nil || final.Content == "" {
		return "", llm.Usage{}, false
	}
	c.emit(Event{Kind: EventStepUsage, Usage: usage})
	return final.Content, usage, true
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
		return s[:n] + "…"
	}
	return s
}
