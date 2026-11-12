package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"minimalharness/internal/agent"
)

// ApprovalRequest is one pending tool approval. The agent goroutine
// blocks on Reply; the model answers it from a y/n keypress. The agent
// never touches Bubble Tea directly — it only calls Approver.Ask.
type ApprovalRequest struct {
	Prompt string
	Reply  chan bool // buffered: the answer never blocks the UI
}

// ApprovalMsg carries a new approval request into the model.
type ApprovalMsg struct {
	Request *ApprovalRequest
}

// Approver bridges the agent's Config.Ask to the TUI program.
type Approver struct {
	program *tea.Program
}

func NewApprover(p *tea.Program) *Approver {
	return &Approver{program: p}
}

// Ask sends the request to the model and blocks until the user answers
// y or n. If the program is gone (quit while waiting), Reply stays
// unread and the process exits anyway — Ask never returns an error.
func (a *Approver) Ask(prompt string) (bool, error) {
	req := &ApprovalRequest{Prompt: prompt, Reply: make(chan bool, 1)}
	a.program.Send(ApprovalMsg{Request: req})
	return <-req.Reply, nil
}

// ConfirmContinuation is the TUI's doom-loop prompt, reusing the
// approval dialog: y continues once, n stops. The dialog is y/n
// only, so "always this session" is not offered here — the CLI and
// the RPC GUI offer all three decisions.
func (a *Approver) ConfirmContinuation(req agent.ContinuationRequest) (agent.ContinuationDecision, error) {
	prompt := fmt.Sprintf("Repeated tool call: %s %s was requested %d times with identical input. Continue once?", req.ToolName, req.InputPreview, req.RepeatCount)
	ok, err := a.Ask(prompt)
	if err != nil {
		return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, err
	}
	if ok {
		return agent.ContinuationDecision{Outcome: agent.ContinuationOnce}, nil
	}
	return agent.ContinuationDecision{Outcome: agent.ContinuationDeny}, nil
}
