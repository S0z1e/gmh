package tools

// PolicyError distinguishes a local policy refusal from an executed command's
// failure. Workspace edits cannot repair an unchanged policy decision.
type PolicyError struct{ Reason string }

func (e *PolicyError) Error() string { return "denied by policy: " + e.Reason }
