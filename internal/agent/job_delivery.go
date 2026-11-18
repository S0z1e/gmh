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

// A later model batch must also wait for an unconfirmed stop. Otherwise a
// retry can run before the old result, recreating the ordering problem that
// deferring only the remainder of the stop's own batch was meant to prevent.
func pendingStopTarget(jobs []*tools.Job) *tools.Job {
	for _, job := range jobs {
		if job.StopRequested() && !job.Delivered() {
			return job
		}
	}
	return nil
}

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
				shares := jobDisplayShares(jobs[:count], toolResultMaxChars-len(prefix)-len(notice)-count)
				var body strings.Builder
				body.WriteString(prefix)
				body.WriteString(notice)
				for i, job := range jobs[:count] {
					body.WriteString(completedJobDisplay(job, shares[i]))
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

// Short results return their unused space to the other results. If all the
// captured previews fit together, none is clipped merely for having a sibling.
func jobDisplayShares(jobs []llm.Message, budget int) []int {
	wanted := make([]int, len(jobs))
	shares := make([]int, len(jobs))
	for i, job := range jobs {
		wanted[i] = len(completedJobDisplay(job, budget))
	}
	remaining := budget
	for remaining > 0 {
		unsatisfied := 0
		for i := range jobs {
			if shares[i] < wanted[i] {
				unsatisfied++
			}
		}
		if unsatisfied == 0 {
			break
		}
		grant := max(1, remaining/unsatisfied)
		for i := range jobs {
			more := min(grant, wanted[i]-shares[i], remaining)
			shares[i] += more
			remaining -= more
		}
	}
	return shares
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
	header := textutil.Prefix(strings.ToValidUTF8(preview.Header, "\uFFFD"), min(128, max(0, limit-1))) + "\n"
	note := strings.ToValidUTF8(preview.Note, "\uFFFD")
	if len(note) > limit/5 {
		note = ""
	}
	output := preview.Head
	if preview.Tail != "" {
		output += "\n... [captured output excerpt] ...\n" + preview.Tail
	}
	path := "Output file: " + strings.ToValidUTF8(m.Observation.OutputFile, "\uFFFD") + "\n"
	// A small result's share includes its full header and path. Do not
	// shrink those again as fractions of that already-sized share.
	wholeFits := len(header)+len(path)+len(output)+len(note) <= limit
	if m.Observation.OutputFile != "" && (len(path) <= limit/4 || wholeFits) {
		header += path
	} else if m.Observation.OutputFile != "" {
		header += "[Output file path retained in session]\n"
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
	remember := func(jobs []checkpoint.Job) {
		for _, job := range jobs {
			if job.Status == checkpoint.Done && job.Delivered && !job.Started.IsZero() {
				k := key(job.ID, job.Started)
				delivered[k] = true
			}
		}
	}
	for _, m := range saved {
		if m.Kind == "checkpoint" {
			state, err := checkpoint.Decode([]byte(m.Content))
			if err != nil {
				return fmt.Errorf("reconcile background deliveries: %w", err)
			}
			remember(state.Jobs)
		}
		if m.Observation != nil {
			remember(m.Observation.Jobs)
			for _, completed := range m.Observation.JobCompletions() {
				remember([]checkpoint.Job{completed.Job})
			}
		}
	}
	for _, job := range jobs {
		if job.Delivered() || !delivered[key(job.ID, job.Started)] {
			continue
		}
		if result, done := job.Result(); done {
			job.MarkDelivered()
			// Every path publishes completion only after a successful append.
			// A readable but unacknowledged write still owes its event.
			output, toolErr := jobResultMessage(job, result)
			c.emit(Event{Kind: EventToolBackgroundDone, ToolName: "bash", ToolOutput: output, ToolErr: toolErr,
				JobID: job.ID, ToolCallID: job.ToolCallID, InvocationID: job.InvocationID,
				ToolExitCode: &result.ExitCode, ToolOutputFile: job.OutputFile})
		}
	}
	return nil
}
