package agent

import (
	"fmt"
	"strings"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/textutil"
	"minimalharness/internal/tools"
)

// The signal was sent, but the original outcome is not available yet.
// This settlement supplies neither command-failure nor command-success evidence.
const stopPendingKind = "stop_pending"

// A stop may settle before its target finishes. Keep the later outcome at that
// stop when the history still contains its anchor, including after cold resume.
func pendingStopCompletion(history []llm.Message, job *tools.Job, res tools.JobResult, output string) llm.Message {
	m := completedJobMessage(job, res, output)
	for i := len(history) - 1; i >= 0; i-- {
		stop := history[i]
		if stop.Kind != stopPendingKind || stop.Observation == nil || stop.InvocationID == "" {
			continue
		}
		if target := stop.Observation.PendingStop; target != nil && target.ID == job.ID && target.Started.Equal(job.Started) {
			m.Observation.BeforeInvocationID = stop.InvocationID
			break
		}
	}
	return m
}

// Keep durable results separate, but show them at their stop call. Provider
// protocols group tool replies before user messages, so a plain job message
// would otherwise appear after a same-batch retry. Large groups overflow into
// bounded displays that explicitly retain their earlier causal position.
func stoppedResultsView(history []llm.Message) []llm.Message {
	anchors := make(map[string]llm.Message)
	for _, m := range history {
		if m.Role == "tool" && m.InvocationID != "" {
			anchors[m.InvocationID] = m
		}
	}
	groups := make(map[string][]llm.Message)
	anchored := func(m llm.Message) bool {
		return m.Kind == "job" && m.Observation != nil && anchors[m.Observation.BeforeInvocationID].InvocationID != ""
	}
	for _, m := range history {
		if anchored(m) {
			key := m.Observation.BeforeInvocationID
			groups[key] = append(groups[key], m)
		}
	}
	var out, overflow []llm.Message
	for _, m := range history {
		if anchored(m) {
			continue
		}
		if m.Role == "assistant" || session.IsTurnStart(m) {
			out = append(out, overflow...)
			overflow = nil
		}
		if jobs := groups[m.InvocationID]; m.Role == "tool" && len(jobs) != 0 {
			prefix := capText(m.Content, 1024)
			notice := fmt.Sprintf("\n[Background outcomes from stop call %s, completed before subsequent tools in that batch.]\n", textutil.Prefix(m.ToolCallID, 80))
			first := true
			for len(jobs) > 0 {
				// At most 32 outcomes share a 32 KiB display. Each keeps
				// its own header and actual head/tail, including the marker.
				count := min(len(jobs), 32)
				share := (toolResultMaxChars - len(prefix) - len(notice) - count) / count
				var body strings.Builder
				body.WriteString(prefix)
				body.WriteString(notice)
				for _, job := range jobs[:count] {
					body.WriteString(completedJobDisplay(job, share))
					body.WriteByte('\n')
				}
				if first {
					m.Content = body.String()
					first = false
				} else {
					overflow = append(overflow, llm.Message{Role: "user", Kind: "job", Content: body.String()})
				}
				prefix = ""
				jobs = jobs[count:]
			}
		}
		out = append(out, m)
	}
	return append(out, overflow...)
}

func boundedJobDisplay(s string, limit int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= limit {
		return s
	}
	const marker = "\n... [head/tail clipped] ...\n"
	remaining := max(0, limit-len(marker))
	head := remaining * 2 / 3
	return textutil.Prefix(s, head) + marker + textutil.Suffix(s, remaining-head)
}

func completedJobDisplay(m llm.Message, limit int) string {
	if m.Observation == nil || m.Observation.JobDisplay == nil {
		return boundedJobDisplay(m.Content, limit)
	}
	preview := m.Observation.JobDisplay
	header := textutil.Prefix(strings.ToValidUTF8(preview.Header, "\uFFFD"), min(128, limit/4)) + "\n"
	path := "Output file: " + m.Observation.OutputFile + "\n"
	if m.Observation.OutputFile != "" && len(path) <= limit/4 {
		header += path
	} else if m.Observation.OutputFile != "" {
		header += "[Output file path retained in session]\n"
	}
	note := strings.ToValidUTF8(preview.Note, "\uFFFD")
	if len(note) > limit/5 {
		note = ""
	}
	output := preview.Head
	if preview.Tail != "" {
		output += "\n... [captured output excerpt] ...\n" + preview.Tail
	}
	return header + boundedJobDisplay(output, limit-len(header)-len(note)) + note
}

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
