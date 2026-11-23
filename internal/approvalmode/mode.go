// Package approvalmode defines the modes accepted by configuration and frontends.
package approvalmode

import "fmt"

const (
	AskForApproval = "ask-for-approval"
	ApproveForMe   = "approve-for-me"
	FullAccess     = "full-access"
	Suggest        = "suggest"
	Auto           = "auto"

	Choices = AskForApproval + ", " + ApproveForMe + ", or " + FullAccess
)

func Validate(mode string) error {
	switch mode {
	case AskForApproval, ApproveForMe, FullAccess, Suggest, Auto:
		return nil
	default:
		return fmt.Errorf("mode %q: use %s (legacy: suggest or auto)", mode, Choices)
	}
}
