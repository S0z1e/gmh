// Package agent runs the single loop: LLM -> tool calls -> observe -> repeat.
// Approval is a commitment/UX boundary, not a sandbox: with bash available,
// denying write/edit can be bypassed. Run on disposable copies or in a container.
package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

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
	}
}

func (c *Config) ask(prompt string) (bool, error) {
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
			c.emit(Event{Kind: EventDone, Text: lastText, Usage: total})
			return lastText, total, nil
		}
		// Execute tool calls in order. (Parallel batches are v0.2; serial
		// avoids two tools writing the same file at once.)
		for _, tc := range assistant.ToolCalls {
			c.emit(Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
			if c.needsApproval(tc.Function.Name) {
				prompt := fmt.Sprintf("Run %s %s?", tc.Function.Name, truncate(tc.Function.Arguments, 200))
				c.emit(Event{Kind: EventApproval, Text: prompt})
				ok, err := c.ask(prompt)
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
			out, err := c.Exec.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
			obs := out
			if err != nil {
				// Tool errors are observations, not fatal errors.
				obs = fmt.Sprintf("Error: %v\nOutput:\n%s", err, out)
			}
			c.emit(Event{Kind: EventToolEnd, ToolName: tc.Function.Name, ToolOutput: obs, ToolErr: err})
			msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
			if c.Session != nil {
				_ = c.Session.Append(msgs[len(msgs)-1])
			}
		}
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
