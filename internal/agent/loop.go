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
	// DenyInsteadOfPrompt refuses every approval without asking (copy of
	// Claude Code's permissions.denyInsteadOfPrompting). -p print mode
	// sets it: an unattended run must never block waiting for a human.
	// It only ever denies, never approves.
	DenyInsteadOfPrompt bool

	// LastPromptTokens is the prompt-token count of the most recent LLM
	// call in the last Run: the current context size, since history is
	// resent in full each time. Used for the compaction trigger.
	LastPromptTokens int
}

func (c *Config) emit(e Event) {
	if c.OnEvent != nil {
		c.OnEvent(e)
	}
}

func ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		llm.Tool("read", "Read a file (with offset/limit) or list a directory.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}`),
		llm.Tool("write", "Create a NEW file. Fails if the file exists; use edit then.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		llm.Tool("edit", "Replace exact oldText (must match once) with newText in an existing file.", `{"type":"object","properties":{"path":{"type":"string"},"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["path","oldText","newText"]}`),
		llm.Tool("bash", "Run a shell command in the workspace root. Timeout in seconds, default 60, max 300.", `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer"}},"required":["command"]}`),
		llm.Tool("grep", "Search file contents with a regex. Prefer this over bash grep/rg. Returns file:line matches, capped (truncation is marked).", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"include":{"type":"string"},"exclude":{"type":"string"},"context":{"type":"integer"},"maxMatches":{"type":"integer"}},"required":["pattern"]}`),
		llm.Tool("glob", "List files matching a glob pattern, e.g. **/*.go. Prefer this over bash find/ls for discovering files.", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"limit":{"type":"integer"}},"required":["pattern"]}`),
		llm.Tool("load_skill", "Load the full text of an available skill by name when its guidance is needed for the current task. The skill names and one-line descriptions are in the system prompt. Pass reference (a file name from the skill's references/ footer) to read one reference file instead of the whole skill.", `{"type":"object","properties":{"name":{"type":"string"},"reference":{"type":"string"}},"required":["name"]}`),
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

func (c *Config) needsApproval(name string) bool {
	if c.Mode == "auto" {
		return false
	}
	return name == "write" || name == "edit" || name == "bash"
}

// execBash runs a bash tool call, backgrounding it when it runs past
// the executor's threshold. It returns the observation for the model
// and, when backgrounded, the job so Run can feed its result back
// later. A call that finishes within the threshold (or with
// backgrounding disabled) returns a nil job and a normal observation.
func (c *Config) execBash(ctx context.Context, tc llm.ToolCall) (string, *tools.Job) {
	threshold := time.Duration(0)
	if c.Exec != nil {
		threshold = c.Exec.BackgroundThreshold
	}
	var a tools.BashArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
		return fmt.Sprintf("Error: bad bash args: %v\nOutput:\n", err), nil
	}
	if threshold <= 0 {
		out, err := c.Exec.Bash(ctx, a)
		if err != nil {
			return fmt.Sprintf("Error: %v\nOutput:\n%s", err, out), nil
		}
		return out, nil
	}
	job, err := c.Exec.BashBackground(ctx, a)
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
	case <-job.Done():
		return deliver()
	case <-time.After(threshold):
		// It may have finished just as the threshold fired: deliver
		// it normally instead of backgrounding a finished job.
		if _, ok := job.Result(); ok {
			return deliver()
		}
		c.emit(Event{Kind: EventToolBackgrounded, ToolName: "bash", ToolArgs: tc.Function.Arguments, JobID: job.ID})
		return fmt.Sprintf("Command is still running in the background (job %s). Continue with other work; its output will be delivered automatically when it finishes. Do not re-run the command, and do not wait for it with sleep or polling.", job.ID), job
	}
}

// pendingJob is one backgrounded bash call waiting for its result.
// A slice, not a map keyed by call id: two calls must never share an
// entry even if a provider ever reuses a call id.
type pendingJob struct {
	callID string
	job    *tools.Job
}

// drainPending waits for the jobs still running and feeds each result
// back as a user message (appended to msgs and the session), then
// returns the extended msgs. The result is a user message, NOT a
// second tool message for the same call id: the call already has its
// tool result (the "still running" observation), and a second one
// with the same id is a 400 from the API (verified live, P2-8). The
// output is capped like a recent tool result: the fed-back message is
// a user message, so truncateToolResults never trims it.
func (c *Config) drainPending(ctx context.Context, msgs []llm.Message, pending []pendingJob) ([]llm.Message, error) {
	jobs := make([]*tools.Job, 0, len(pending))
	jobIDs := make([]string, 0, len(pending))
	for _, p := range pending {
		jobs = append(jobs, p.job)
		jobIDs = append(jobIDs, p.job.ID)
	}
	// Only announce the wait when something is actually still running.
	waiting := false
	for _, job := range jobs {
		if _, ok := job.Result(); !ok {
			waiting = true
			break
		}
	}
	if waiting {
		c.emit(Event{Kind: EventBackgroundWait, Text: strings.Join(jobIDs, ", ")})
	}
	for _, job := range jobs {
		select {
		case <-job.Done():
		case <-ctx.Done():
			return msgs, ctx.Err()
		}
		res, _ := job.Result()
		var obs string
		var toolErr error
		if res.Err != nil {
			obs = fmt.Sprintf("Background job %s finished with error: %v (exit %d).\nOutput:\n%s", job.ID, res.Err, res.ExitCode, capText(res.Output, 32*1024))
			toolErr = res.Err
		} else {
			obs = fmt.Sprintf("Background job %s finished (exit %d).\nOutput:\n%s", job.ID, res.ExitCode, capText(res.Output, 32*1024))
		}
		c.emit(Event{Kind: EventToolBackgroundDone, ToolName: "bash", ToolOutput: obs, ToolErr: toolErr, JobID: job.ID})
		msgs = append(msgs, llm.Message{Role: "user", Content: obs})
		if c.Session != nil {
			_ = c.Session.Append(msgs[len(msgs)-1])
		}
	}
	return msgs, nil
}

// Run executes one user turn. It appends messages to the session store
// and returns the assistant's final text, total tokens used, and error.
func (c *Config) Run(ctx context.Context, history []llm.Message) (string, llm.Usage, error) {
	steps := c.MaxSteps
	if steps <= 0 {
		steps = 50
	}
	msgs := append([]llm.Message{{Role: "system", Content: c.System}}, history...)
	var total llm.Usage
	var lastText string
	// pending background bash jobs, in call order. Their results are
	// fed back as user messages when the turn would otherwise end
	// (see drainPending).
	var pending []pendingJob

	for step := 0; step < steps; step++ {
		var assistant llm.Message
		var usage llm.Usage
		// Send the truncated view to the LLM; the full history stays
		// in msgs and in the session file.
		apiMsgs := truncateToolResults(msgs, 2, 2000, 32*1024)
		err := c.Client.Stream(ctx, apiMsgs, c.Tools, func(s string) {
			c.emit(Event{Kind: EventTextDelta, Text: s})
		}, func(m llm.Message, u llm.Usage) {
			assistant = m
			usage = u
		})
		if err != nil {
			wrapped := fmt.Errorf("step %d: %w", step+1, err)
			// The turn is over, but a running job's result is still
			// the user's data: deliver it before reporting the error
			// so it is not silently lost.
			if len(pending) > 0 {
				_, _ = c.drainPending(ctx, msgs, pending)
			}
			c.emit(Event{Kind: EventError, Err: wrapped})
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
				c.emit(Event{Kind: EventDone, Text: lastText, Usage: total})
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
		for _, tc := range assistant.ToolCalls {
			c.emit(Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
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
					c.emit(Event{Kind: EventError, Err: err})
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
			if tc.Function.Name == "bash" {
				var job *tools.Job
				obs, job = c.execBash(ctx, tc)
				if job != nil {
					pending = append(pending, pendingJob{callID: tc.ID, job: job})
				}
			} else {
				out, dispatchErr := c.Exec.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
				obs = out
				err = dispatchErr
				if err != nil {
					// Tool errors are observations, not fatal errors.
					obs = fmt.Sprintf("Error: %v\nOutput:\n%s", err, out)
				}
			}
			c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs, ToolErr: err})
			msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
			if c.Session != nil {
				_ = c.Session.Append(msgs[len(msgs)-1])
			}
		}
	}
	// The step cap hit with jobs still running: deliver their results
	// before reporting the cap, so the work is not silently lost.
	if len(pending) > 0 {
		_, _ = c.drainPending(ctx, msgs, pending)
	}
	capped := fmt.Errorf("step cap reached (%d); the task was not finished", steps)
	c.emit(Event{Kind: EventError, Err: capped})
	return lastText, total, capped
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
