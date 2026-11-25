package agent

import (
	"fmt"
	"sort"
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
	notice := textutil.Prefix(strings.ToValidUTF8(preview.Notice, "\uFFFD"), 128)
	note := strings.ToValidUTF8(preview.Note, "\uFFFD")
	output := preview.Head
	if preview.Tail != "" {
		output += "\n... [captured output excerpt] ...\n" + preview.Tail
	}
	path := ""
	if m.Observation.OutputFile != "" {
		path = "Output file: " + strings.ToValidUTF8(m.Observation.OutputFile, "\uFFFD") + "\n"
	}
	// A small result's share includes its full header and path. Do not
	// shrink those again as fractions of that already-sized share.
	wholeFits := len(header)+len(path)+len(output)+len(note)+len(notice) <= limit
	if !wholeFits && len(note) > limit/5 {
		note = ""
	}
	if m.Observation.OutputFile != "" && (len(path) <= limit/4 || wholeFits) {
		header += path
	} else if m.Observation.OutputFile != "" {
		header += "[Output file path retained in session]\n"
	}
	return header + boundedJobDisplay(output, limit-len(header)-len(note)-len(notice)) + notice + note
}

func backgroundDoneEvent(job *tools.Job, res tools.JobResult, obs string, toolErr error) Event {
	var exitCode *int
	if !res.ExitCodeUnknown {
		code := res.ExitCode
		exitCode = &code
	}
	return Event{
		Kind:           EventToolBackgroundDone,
		ToolName:       "bash",
		ToolOutput:     obs,
		ToolErr:        toolErr,
		JobID:          job.ID,
		ToolCallID:     job.ToolCallID,
		InvocationID:   job.InvocationID,
		ToolExitCode:   exitCode,
		ToolOutputFile: job.OutputFile,
	}
}

func unknownJobMessage(job *tools.Job, res tools.JobResult) llm.Message {
	snap := job.Snapshot()
	note := snap.RecoveryNote
	if note == "" && res.Err != nil {
		note = res.Err.Error()
	}
	outInfo := ""
	if snap.OutputFile != "" {
		outInfo = fmt.Sprintf(" Pre-crash output file: %s.", snap.OutputFile)
	}
	body := fmt.Sprintf("Recovered background job %s (command: %q, cwd: %q) has status unknown: %s.%s\nDo not automatically replay this command because side effects may already have occurred; inspect the output file or workspace state and stop or confirm with the user before running anything similar.\nOutput so far:\n%s",
		snap.ID, snap.Command, snap.Cwd, note, outInfo, capText(strings.ToValidUTF8(res.Output, "\uFFFD"), 32*1024))
	cpJob := snapshotJob(job, false)
	cpJob.UnknownNoticed = true
	obs := &llm.ToolObservation{
		Tool:        "bash",
		Fingerprint: factDigest(fmt.Sprintf("unknown-job:%s:%d:%s", snap.ID, snap.Started.UnixNano(), snap.Command)),
		Command:     snap.Command,
		Cwd:         snap.Cwd,
		OutputFile:  snap.OutputFile,
		Recovery:    true,
		Jobs:        []checkpoint.Job{cpJob},
	}
	if ex, err := checkpoint.Observe(strings.ToValidUTF8(note+"\n"+res.Output, "\uFFFD"), 4096); err == nil {
		obs.Failure = &ex
	}
	return llm.Message{Role: "user", Kind: "job", InvocationID: snap.InvocationID, Content: body, ToolError: true, Observation: obs}
}

func adoptedJobNoticeMessage(job *tools.Job) llm.Message {
	snap := job.Snapshot()
	outInfo := ""
	if snap.OutputFile != "" {
		outInfo = fmt.Sprintf(" Output file: %s.", snap.OutputFile)
	}
	note := snap.RecoveryNote
	if note != "" {
		note = " (" + note + ")"
	}
	body := fmt.Sprintf("Recovered background job %s is still running (adopted PID %d, pgid %d, cwd: %q, command: %q).%s%s Wait for it to finish or stop it with bash kill_job_id=%q; do not replay the command.",
		snap.ID, snap.Identity.PID, snap.Identity.PGID, snap.Cwd, snap.Command, note, outInfo, snap.ID)
	cpJob := snapshotJob(job, false)
	cpJob.AdoptionNoticed = true
	obs := &llm.ToolObservation{
		Tool:       "bash",
		Command:    snap.Command,
		Cwd:        snap.Cwd,
		OutputFile: snap.OutputFile,
		Recovery:   true,
		Jobs:       []checkpoint.Job{cpJob},
	}
	return llm.Message{Role: "user", Kind: "job", InvocationID: snap.InvocationID, Content: body, Observation: obs}
}

// An append may write a complete result before Sync reports an error. Reconcile
// that durable history before retrying delivery, including older stop records.
func (c *Config) reconcileJobDeliveries() error {
	if c.Exec == nil || c.Session == nil {
		return nil
	}
	jobs := c.Exec.Jobs()
	needsReconcile := false
	for _, job := range jobs {
		snap := job.Snapshot()
		switch snap.Status {
		case tools.JobDone, tools.JobUnknown:
			if !snap.Delivered {
				needsReconcile = true
			}
		case tools.JobRunning:
			if snap.Adopted && !snap.AdoptionNoticed {
				needsReconcile = true
			}
		}
		if needsReconcile {
			break
		}
	}
	if !needsReconcile {
		return nil
	}
	saved, err := session.Load(c.Session.ID)
	if err != nil {
		return fmt.Errorf("reconcile background deliveries: %w", err)
	}
	delivered, err := recordedJobDeliveries(saved)
	if err != nil {
		return fmt.Errorf("reconcile background deliveries: %w", err)
	}
	for _, job := range jobs {
		snap := job.Snapshot()
		rec := delivered[jobDeliveryKey(snap.ID, snap.Started)]
		switch snap.Status {
		case tools.JobDone:
			if snap.Delivered || !rec.doneDelivered {
				continue
			}
			job.MarkDelivered()
			// Every path publishes completion only after a successful append.
			// A readable but unacknowledged write still owes its event.
			output, toolErr := jobResultMessage(job, snap.Result)
			c.emit(backgroundDoneEvent(job, snap.Result, output, toolErr))
		case tools.JobUnknown:
			if snap.Delivered || !rec.unknownNoticed {
				continue
			}
			job.MarkDelivered()
		case tools.JobRunning:
			if snap.Adopted && !snap.AdoptionNoticed && rec.adoptionNoticed {
				job.MarkAdoptionNoticed()
				job.MarkReported()
			}
		}
	}
	return nil
}

type jobIncarnation struct{ id, started string }

type jobDeliveryState struct {
	doneDelivered   bool
	unknownNoticed  bool
	adoptionNoticed bool
}

func jobDeliveryKey(id string, started time.Time) jobIncarnation {
	return jobIncarnation{id, started.UTC().Format(time.RFC3339Nano)}
}

func recordedJobDeliveries(saved []llm.Message) (map[jobIncarnation]jobDeliveryState, error) {
	delivered := make(map[jobIncarnation]jobDeliveryState)
	rememberCheckpoint := func(jobs []checkpoint.Job) {
		for _, job := range jobs {
			if job.Started.IsZero() {
				continue
			}
			k := jobDeliveryKey(job.ID, job.Started)
			st := delivered[k]
			if job.Status == checkpoint.Done && job.Delivered {
				st.doneDelivered = true
			}
			if job.UnknownNoticed {
				st.unknownNoticed = true
			}
			if job.AdoptionNoticed {
				st.adoptionNoticed = true
			}
			if st != (jobDeliveryState{}) {
				delivered[k] = st
			}
		}
	}
	for _, m := range saved {
		if m.Kind == "checkpoint" {
			state, err := checkpoint.Decode([]byte(m.Content))
			if err != nil {
				return nil, err
			}
			rememberCheckpoint(state.Jobs)
		}
		if m.Observation != nil {
			for _, job := range m.Observation.Jobs {
				if job.Started.IsZero() {
					continue
				}
				k := jobDeliveryKey(job.ID, job.Started)
				st := delivered[k]
				if job.Status == checkpoint.Done && job.Delivered {
					st.doneDelivered = true
				}
				if job.UnknownNoticed || (m.Kind == "job" && job.Status == checkpoint.Unknown) {
					st.unknownNoticed = true
				}
				if job.AdoptionNoticed || (m.Kind == "job" && m.Observation.Recovery && !m.ToolError && job.Status == checkpoint.Running) {
					st.adoptionNoticed = true
				}
				if st != (jobDeliveryState{}) {
					delivered[k] = st
				}
			}
			for _, completed := range m.Observation.JobCompletions() {
				if completed.Job.Status == checkpoint.Done && completed.Job.Delivered && !completed.Job.Started.IsZero() {
					k := jobDeliveryKey(completed.Job.ID, completed.Job.Started)
					st := delivered[k]
					st.doneDelivered = true
					delivered[k] = st
				}
			}
		}
	}
	return delivered, nil
}

// SaveJobResults settles finished and unknown jobs while their owning session
// writer is still open. Call between turns; it performs no model request or event callback.
// A failed append leaves its job undelivered so shutdown can be retried.
func SaveJobResults(store *session.Store, jobs []*tools.Job) error {
	_, err := SaveJobResultsMessages(store, jobs)
	return err
}

// SaveJobResultsMessages settles finished and unknown jobs like SaveJobResults
// and returns any transcript messages newly appended to store so interactive
// callers can synchronize in-memory history.
func SaveJobResultsMessages(store *session.Store, jobs []*tools.Job) ([]llm.Message, error) {
	var ready []*tools.Job
	for _, job := range jobs {
		status := job.Status()
		if (status == tools.JobDone || status == tools.JobUnknown) && !job.Delivered() {
			ready = append(ready, job)
		}
	}
	if len(ready) == 0 {
		return nil, nil
	}
	saved, err := session.Load(store.ID)
	if err != nil {
		return nil, err
	}
	delivered, err := recordedJobDeliveries(saved)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ready, func(i, j int) bool {
		if !ready[i].Started.Equal(ready[j].Started) {
			return ready[i].Started.Before(ready[j].Started)
		}
		return ready[i].ID < ready[j].ID
	})
	var appended []llm.Message
	for _, job := range ready {
		rec := delivered[jobDeliveryKey(job.ID, job.Started)]
		switch job.Status() {
		case tools.JobDone:
			if !rec.doneDelivered {
				result, _ := job.Result()
				output, _ := jobResultMessage(job, result)
				m := pendingStopCompletion(saved, job, result, output)
				if err := store.Append(m); err != nil {
					return appended, err
				}
				saved = append(saved, m)
				appended = append(appended, m)
			}
			job.MarkDelivered()
		case tools.JobUnknown:
			if !rec.unknownNoticed {
				res, _ := job.UnknownResult()
				m := unknownJobMessage(job, res)
				if err := store.Append(m); err != nil {
					return appended, err
				}
				saved = append(saved, m)
				appended = append(appended, m)
			}
			job.MarkDelivered()
		}
	}
	return appended, nil
}
