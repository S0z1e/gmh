package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"minimalharness/internal/input"
	"minimalharness/internal/tools"
)

var defaultPromptInput = input.New(os.Stdin)

// ApprovalRequest carries the exact invocation the user is approving. Renderers
// may paginate it, but must not recover arguments from a truncated preview.
type ApprovalRequest struct {
	// Context bounds the prompt; implementations must stop waiting when it ends.
	Context      context.Context `json:"-"`
	ToolCallID   string
	TargetPath   string
	InvocationID string
	ToolName     string
	Arguments    string
}

func (r ApprovalRequest) Prompt() string {
	prompt := fmt.Sprintf("Run %s %s?", r.ToolName, r.Arguments)
	if r.ToolName == tools.GitStatusTool {
		prompt += "\nRead Git status on the host, including linked worktree metadata if needed."
	}
	if r.TargetPath != "" {
		prompt += "\nResolved file target: " + r.TargetPath
	}
	return prompt
}

func (c *Config) ask(ctx context.Context, req ApprovalRequest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	req.Context = ctx
	if c.Ask != nil {
		return c.Ask(req)
	}
	fmt.Printf("%s [y/N] ", req.Prompt())
	line, err := defaultPromptInput.Read(ctx)
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}
