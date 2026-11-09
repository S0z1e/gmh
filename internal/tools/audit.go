package tools

import "time"

// AuditEntry is one auditable event: an approval decision, a bash
// execution, or a denylist denial. The caller (app.Setup) wires
// Executor.Audit to the session store's sidecar file; audit failures
// must never break the loop, so writers ignore them.
//
// ExitCode is -1 when the tool did not run (denied) or did not exit
// (timed out); otherwise it is the process exit code (0 = success).
type AuditEntry struct {
	TS       time.Time `json:"ts"`
	Kind     string    `json:"kind"` // "approval", "bash", "denied"
	Tool     string    `json:"tool"`
	Args     string    `json:"args,omitempty"`
	Decision string    `json:"decision,omitempty"` // "approved" | "denied"
	ExitCode int       `json:"exit_code"`
	Error    string    `json:"error,omitempty"`
}
