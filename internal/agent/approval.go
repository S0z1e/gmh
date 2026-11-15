package agent

import "fmt"

// ApprovalRequest carries the exact invocation the user is approving. Renderers
// may paginate it, but must not recover arguments from a truncated preview.
type ApprovalRequest struct {
	ToolName  string
	Arguments string
}

func (r ApprovalRequest) Prompt() string {
	return fmt.Sprintf("Run %s %s?", r.ToolName, r.Arguments)
}
