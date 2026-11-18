package agent

import (
	"fmt"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/session"
)

// An append may write a complete result before Sync reports an error. Reconcile
// that durable history before retrying delivery, including older stop records.
func (c *Config) reconcileJobDeliveries() error {
	if c.Exec == nil || c.Session == nil {
		return nil
	}
	jobs := c.Exec.Jobs()
	hasUndelivered := false
	for _, job := range jobs {
		if _, done := job.Result(); done && !job.Delivered() {
			hasUndelivered = true
			break
		}
	}
	if !hasUndelivered {
		return nil
	}
	saved, err := session.Load(c.Session.ID)
	if err != nil {
		return fmt.Errorf("reconcile background deliveries: %w", err)
	}
	type incarnation struct{ id, started string }
	key := func(id string, started time.Time) incarnation {
		return incarnation{id, started.UTC().Format(time.RFC3339Nano)}
	}
	delivered := make(map[incarnation]bool)
	notify := make(map[incarnation]bool)
	remember := func(jobs []checkpoint.Job, stopped bool) {
		for _, job := range jobs {
			if job.Status == checkpoint.Done && job.Delivered && !job.Started.IsZero() {
				k := key(job.ID, job.Started)
				delivered[k] = true
				notify[k] = notify[k] || stopped
			}
		}
	}
	for _, m := range saved {
		if m.Kind == "checkpoint" {
			state, err := checkpoint.Decode([]byte(m.Content))
			if err != nil {
				return fmt.Errorf("reconcile background deliveries: %w", err)
			}
			remember(state.Jobs, false)
		}
		if m.Observation != nil {
			remember(m.Observation.Jobs, m.Observation.BeforeInvocationID != "")
			for _, completed := range m.Observation.JobCompletions() {
				remember([]checkpoint.Job{completed.Job}, true)
			}
		}
	}
	for _, job := range jobs {
		if job.Delivered() || !delivered[key(job.ID, job.Started)] {
			continue
		}
		if result, done := job.Result(); done {
			job.MarkDelivered()
			// Stop emits completion only after a successful append. Other
			// delivery paths already emitted it before attempting the write.
			if !notify[key(job.ID, job.Started)] {
				continue
			}
			output, toolErr := jobResultMessage(job, result)
			c.emit(Event{Kind: EventToolBackgroundDone, ToolName: "bash", ToolOutput: output, ToolErr: toolErr,
				JobID: job.ID, ToolCallID: job.ToolCallID, InvocationID: job.InvocationID,
				ToolExitCode: &result.ExitCode, ToolOutputFile: job.OutputFile})
		}
	}
	return nil
}
