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
	OnText   func(string)
	Ask      func(prompt string) (bool, error) // nil = use stdin prompt
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
		err := c.Client.Stream(ctx, msgs, c.Tools, c.OnText, func(m llm.Message, u llm.Usage) {
			assistant = m
			usage = u
		})
		if err != nil {
			return lastText, total, fmt.Errorf("step %d: %w", step+1, err)
		}
		total.PromptTokens += usage.PromptTokens
		total.CompletionTokens += usage.CompletionTokens
		total.TotalTokens += usage.TotalTokens
		if c.OnText != nil && assistant.Content != "" {
			// Stream already printed deltas; print a newline separator.
			fmt.Println()
		}
		lastText = assistant.Content
		msgs = append(msgs, assistant)
		if c.Session != nil {
			_ = c.Session.Append(assistant)
		}
		if len(assistant.ToolCalls) == 0 {
			return lastText, total, nil
		}
		// Execute tool calls in order. (Parallel batches are v0.2; serial
		// avoids two tools writing the same file at once.)
		for _, tc := range assistant.ToolCalls {
			if c.needsApproval(tc.Function.Name) {
				ok, err := c.ask(fmt.Sprintf("Run %s %s?", tc.Function.Name, truncate(tc.Function.Arguments, 200)))
				if err != nil {
					return lastText, total, err
				}
				if !ok {
					obs := "User denied this tool call. Do not retry it; explain and propose an alternative."
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
			msgs = append(msgs, llm.Message{Role: "tool", Content: obs, ToolCallID: tc.ID})
			if c.Session != nil {
				_ = c.Session.Append(msgs[len(msgs)-1])
			}
		}
	}
	return lastText, total, fmt.Errorf("step cap reached (%d); the task was not finished", steps)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
