package agent

import "fmt"

// ApprovalRequest carries the exact invocation the user is approving. Renderers
// may paginate it, but must not recover arguments from a truncated preview.
type ApprovalRequest struct {
	ToolCallID   string
	TargetPath   string
	InvocationID string
	ToolName     string
	Arguments    string
}

func (r ApprovalRequest) Prompt() string {
	prompt := fmt.Sprintf("Run %s %s?", r.ToolName, r.Arguments)
	if r.TargetPath != "" {
		prompt += "\nResolved file target: " + r.TargetPath
	}
	return prompt
}
