package llm

import "minimalharness/internal/checkpoint"

// ToolObservation is durable harness metadata, omitted by every provider wire
// adapter. It records actual execution, independently of display hooks or clips.
// These historical observations grant no permission or current verification.
type ToolObservation struct {
	Tool        string              `json:"tool"`
	Fingerprint string              `json:"fingerprint,omitempty"`
	Command     string              `json:"command,omitempty"`
	Cwd         string              `json:"cwd,omitempty"`
	OutputFile  string              `json:"output_file,omitempty"`
	Failure     *checkpoint.Excerpt `json:"failure,omitempty"`
	// Answer is present only for an actual human reply, before display hooks.
	// A missing value is not evidence that a human accepted anything.
	Answer       *string          `json:"answer,omitempty"`
	Jobs         []checkpoint.Job `json:"jobs,omitempty"`
	CompletedJob *JobCompletion   `json:"completed_job,omitempty"`
}

// JobCompletion records the original bash outcome when a stop call delivers
// it. The stop call and the background command have different fingerprints.
type JobCompletion struct {
	Job         checkpoint.Job      `json:"job"`
	Fingerprint string              `json:"fingerprint"`
	Cwd         string              `json:"cwd,omitempty"`
	Failure     *checkpoint.Excerpt `json:"failure,omitempty"`
}
