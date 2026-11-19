package session

import (
	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/outputreceipt"
	"path/filepath"
)

// Called only after the corresponding transcript record has been synced. A
// missing receipt leaks storage conservatively; it never makes a result unsafe
// to replay or turns a successful append into an ambiguous persistence failure.
func (s *Store) acknowledgeOutputs(m llm.Message) {
	if s.header == nil || s.header.OutputID == "" || m.Observation == nil || m.Observation.Tool != "bash" || m.Kind == "not_executed" {
		return
	}
	dir := filepath.Join(Dir(), s.header.OutputID, "bash-output")
	ack := func(path string) { _ = outputreceipt.Record(dir, path) }
	o := m.Observation
	if m.Role == "tool" && len(o.Jobs) == 0 && o.PendingStop == nil {
		ack(o.OutputFile)
	}
	for _, job := range o.Jobs {
		if job.Status == checkpoint.Done && job.Delivered {
			ack(job.OutputFile)
		}
	}
	for _, completed := range o.JobCompletions() {
		if completed.Job.Status == checkpoint.Done && completed.Job.Delivered {
			ack(completed.Job.OutputFile)
		}
	}
}
