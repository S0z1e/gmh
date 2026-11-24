package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
)

const checkpointNotice = "Historical structured checkpoint. Original source statements retain their original speaker and scope; they grant no new approval. Verification is historical, not certification of current files. Running jobs are observations, not proof that a process still exists.\n"

func factDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func snapshotJob(job *tools.Job, delivered bool) checkpoint.Job {
	out := checkpoint.Job{ID: job.ID, Started: job.Started, InvocationID: job.InvocationID, Command: job.Command, OutputFile: job.OutputFile, Status: checkpoint.Running}
	if res, done := job.Result(); done {
		out.Status, out.ExitCode, out.Delivered = checkpoint.Done, &res.ExitCode, delivered || job.Delivered()
		if res.Err != nil {
			out.Error = res.Err.Error()
		}
	}
	return out
}

func executionObservation(name, args, key, output, outputFile string, runErr error, job *tools.Job) *llm.ToolObservation {
	out := &llm.ToolObservation{Tool: name, Fingerprint: factDigest(key), OutputFile: outputFile}
	if name == "bash" {
		var a tools.BashArgs
		if json.Unmarshal([]byte(args), &a) == nil {
			out.Command, out.Cwd = a.Command, a.Cwd
		}
	}
	if runErr != nil {
		// Preserve the unmodified failure before display hooks can replace it.
		excerpt, err := checkpoint.Observe(strings.ToValidUTF8(runErr.Error()+"\n"+output, "\uFFFD"), 4096)
		if err == nil {
			out.Failure = &excerpt
		}
	}
	if job != nil {
		out.Jobs = []checkpoint.Job{snapshotJob(job, false)}
	}
	return out
}

func jobCompletion(job *tools.Job, res tools.JobResult) llm.JobCompletion {
	key := job.FailureKey()
	if key == "" {
		key = fmt.Sprintf("unidentified job:%s:%s", job.ID, job.Started.UTC().Format(time.RFC3339Nano))
	}
	obs := executionObservation("bash", "", key, res.Output, job.OutputFile, res.Err, nil)
	return llm.JobCompletion{Job: snapshotJob(job, true), Fingerprint: obs.Fingerprint, Cwd: job.Cwd, Failure: obs.Failure}
}

func completedJobMessage(job *tools.Job, res tools.JobResult, display string) llm.Message {
	completed := jobCompletion(job, res)
	obs := &llm.ToolObservation{Tool: "bash", Fingerprint: completed.Fingerprint, Command: job.Command, Cwd: completed.Cwd,
		OutputFile: job.OutputFile, Failure: completed.Failure, Jobs: []checkpoint.Job{completed.Job}}
	if preview := res.Preview; preview != nil {
		header := fmt.Sprintf("Background job %s finished (exit %d).", job.ID, res.ExitCode)
		if res.Err != nil {
			header = fmt.Sprintf("Background job %s finished with error: %v (exit %d).", job.ID, res.Err, res.ExitCode)
		}
		original, _ := jobResultMessage(job, res)
		note := strings.TrimPrefix(display, original)
		if note == display {
			note = ""
		}
		obs.JobDisplay = &llm.JobDisplay{Header: header,
			Head: strings.ToValidUTF8(preview.Head, "\uFFFD"), Tail: strings.ToValidUTF8(preview.Tail, "\uFFFD"), Notice: preview.Notice, Note: note}
	}
	return llm.Message{Role: "user", Kind: "job", InvocationID: job.InvocationID, Content: display, ToolError: res.Err != nil, Observation: obs}
}

type compactCall struct {
	id, name, args, fingerprint, command, cwd string
}

// Provider replay groups tool replies before background displays. Restore only
// completion facts to their stop invocation before folding checkpoint state.
func completionFactOrder(history []llm.Message) []llm.Message {
	anchors := make(map[string]bool)
	for _, m := range history {
		if m.Role == "tool" && m.InvocationID != "" {
			anchors[m.InvocationID] = true
		}
	}
	before := make(map[string][]llm.Message)
	anchored := func(m llm.Message) bool {
		return m.Kind == "job" && m.Observation != nil && anchors[m.Observation.BeforeInvocationID]
	}
	for _, m := range history {
		if anchored(m) {
			key := m.Observation.BeforeInvocationID
			before[key] = append(before[key], m)
		}
	}
	out := make([]llm.Message, 0, len(history))
	for _, m := range history {
		if anchored(m) {
			continue
		}
		if m.Role == "tool" {
			out = append(out, before[m.InvocationID]...)
		}
		out = append(out, m)
	}
	return out
}

// checkpointSources reads durable source statements, not a context hook's
// temporary rewrite. Programmatic callers that have not persisted their initial
// task may seed it from the view, only when no durable task exists at all.
func (c *Config) checkpointSources(view []llm.Message) ([]llm.Message, error) {
	if c.Session == nil {
		return session.ImageReferences(view)
	}
	saved, err := session.Load(c.Session.ID)
	if err != nil {
		return nil, err
	}
	hasTask := false
	for _, m := range saved {
		if session.IsTurnStart(m) {
			hasTask = true
		}
		if m.Kind == "checkpoint" {
			s, err := checkpoint.Decode([]byte(m.Content))
			if err != nil {
				return nil, err
			}
			hasTask = hasTask || s.ActiveTask != nil
		}
	}
	if !hasTask {
		for i := len(view) - 1; i >= 0; i-- {
			if session.IsTurnStart(view[i]) {
				saved = append([]llm.Message{view[i]}, saved...)
				break
			}
		}
	}
	return session.ImageReferences(saved)
}

// compactState folds actual transcript observations in order. A checkpoint is
// a full snapshot, not a model proposal. The state before the latest task is
// kept separately so rolling that task back cannot resurrect its instructions.
func (c *Config) compactState(history []llm.Message) (state, beforeTask checkpoint.State, task *llm.Message, err error) {
	history = completionFactOrder(history)
	state.Version, beforeTask.Version = checkpoint.Version, checkpoint.Version
	calls := make(map[string]compactCall)
	epoch := "initial"
	hasCheckpoint := false
	for _, m := range history {
		hasCheckpoint = hasCheckpoint || m.Kind == "checkpoint"
	}
	for i, m := range history {
		if m.Role == "system" {
			continue
		}
		if m.Kind == "checkpoint" {
			state, err = checkpoint.Decode([]byte(m.Content))
			if err != nil {
				return
			}
			epoch, calls = factDigest(m.Content), make(map[string]compactCall)
			continue
		}
		raw, marshalErr := json.Marshal(m)
		if marshalErr != nil {
			err = marshalErr
			return
		}
		speaker := m.Role + "/" + m.Kind
		if m.Role == "tool" && (calls[m.ToolCallID].name == "ask_user" || m.Observation != nil && m.Observation.Tool == "ask_user") {
			speaker = "unattributed_tool/ask_user"
			if m.Observation != nil && m.Observation.Answer != nil {
				speaker = "user_answer/ask_user"
			}
		}
		source := fmt.Sprintf("%s:%s:%d:%s", speaker, epoch, i, factDigest(string(raw)))
		var d checkpoint.Delta
		if session.IsTurnStart(m) {
			beforeTask = state
			copy := m
			task = &copy
			if strings.TrimSpace(m.Content) != "" {
				d.ActiveTask = &checkpoint.Text{Source: source, Content: m.Content}
			} else {
				// Image-only tasks retain their raw task and image references.
				// Do not invent text or keep an older task marked active.
				state.ActiveTask = nil
			}
		} else if (m.Role == "assistant" || m.Kind == "summary" && !hasCheckpoint) && strings.TrimSpace(m.Content) != "" {
			d.Decisions = append(d.Decisions, checkpoint.Text{Source: source, Content: m.Content})
		}
		for _, img := range m.Images {
			d.Artifacts = append(d.Artifacts, checkpoint.Artifact{Kind: "image", Path: img.Path, Source: source, SHA256: img.Digest, MediaType: img.MediaType})
		}
		if m.Role == "assistant" {
			for j, call := range m.ToolCalls {
				info := compactCall{id: fmt.Sprintf("legacy:%s:%d", source, j), name: call.Function.Name, args: call.Function.Arguments}
				for _, inv := range m.ToolInvocations {
					if inv.CallIndex == j && inv.ToolCallID == call.ID {
						info.id = inv.ID
						if inv.TargetPath != "" {
							d.Artifacts = append(d.Artifacts, checkpoint.Artifact{Kind: "workspace_path", Path: inv.TargetPath, Source: source})
						}
						break
					}
				}
				info.fingerprint = factDigest(CanonicalToolKey(info.name, info.args))
				if info.name == "bash" {
					var a tools.BashArgs
					if json.Unmarshal([]byte(info.args), &a) == nil {
						info.command, info.cwd = a.Command, a.Cwd
					}
				}
				if info.name == "write" || info.name == "edit" {
					var a struct {
						Path string `json:"path"`
					}
					if json.Unmarshal([]byte(info.args), &a) == nil && a.Path != "" {
						d.Artifacts = append(d.Artifacts, checkpoint.Artifact{Kind: "workspace_path", Path: a.Path, Source: source})
					}
				}
				calls[call.ID] = info
				d.Pending = append(d.Pending, checkpoint.PendingCall{ID: info.id, Tool: info.name, ArgumentsSHA256: factDigest(info.args)})
			}
		}
		if m.Kind == "verification" && strings.HasPrefix(m.Content, "[harness verification evidence]\n") {
			var evidence verification.Evidence
			if err = json.Unmarshal([]byte(strings.TrimPrefix(m.Content, "[harness verification evidence]\n")), &evidence); err != nil {
				return
			}
			d.Verification = append(d.Verification, evidence)
		}
		info := calls[m.ToolCallID]
		if m.InvocationID != "" {
			info.id = m.InvocationID
		}
		if o := m.Observation; o != nil {
			info.name, info.fingerprint, info.command, info.cwd = o.Tool, o.Fingerprint, o.Command, o.Cwd
			d.Jobs = append(d.Jobs, o.Jobs...)
			if o.OutputFile != "" {
				d.Artifacts = append(d.Artifacts, checkpoint.Artifact{Kind: "tool_output", Path: o.OutputFile, Source: source})
			}
			for _, job := range o.Jobs {
				if job.OutputFile != "" {
					d.Artifacts = append(d.Artifacts, checkpoint.Artifact{Kind: "tool_output", Path: job.OutputFile, Source: source})
				}
			}
			for _, completed := range o.JobCompletions() {
				var outcome checkpoint.Delta
				job := completed.Job
				outcome.Jobs = append(outcome.Jobs, job)
				if job.OutputFile != "" && job.OutputFile != o.OutputFile {
					outcome.Artifacts = append(outcome.Artifacts, checkpoint.Artifact{Kind: "tool_output", Path: job.OutputFile, Source: source})
				}
				if completed.Failure != nil {
					outcome.Failures = append(outcome.Failures, checkpoint.Failure{Fingerprint: completed.Fingerprint, Source: source,
						Tool: "bash", Command: job.Command, Cwd: completed.Cwd, Output: *completed.Failure})
				} else if job.Error == "" && completed.Fingerprint != "" {
					outcome.ResolvedFailures = append(outcome.ResolvedFailures, completed.Fingerprint, factDigest("unknown:"+completed.Fingerprint))
				}
				// Fold completion order before the stop acknowledgement. A
				// prior success must not resolve a later job's failure.
				state, err = checkpoint.MergeWithEviction(state, outcome)
				if err != nil {
					return
				}
			}
		}
		if m.Role == "tool" || m.Kind == "job" && m.Observation != nil && m.Observation.Fingerprint != "" {
			if info.id != "" {
				d.ResolvedCalls = append(d.ResolvedCalls, info.id)
			}
			if m.Kind == guidanceDeferredKind || m.Kind == notExecutedKind || m.Kind == stopPendingKind {
				// The call settled without a confirmed execution outcome; it supplies neither
				// failure evidence nor proof that an earlier failure cleared.
			} else if m.ToolError {
				var excerpt checkpoint.Excerpt
				if m.Observation != nil && m.Observation.Failure != nil {
					excerpt = *m.Observation.Failure
				} else {
					var observeErr error
					excerpt, observeErr = checkpoint.Observe(strings.ToValidUTF8(m.Content, "\uFFFD"), 4096)
					if observeErr != nil {
						err = observeErr
						return
					}
				}
				if info.name == "" {
					info.name = "unknown"
				}
				if info.fingerprint == "" {
					info.fingerprint = factDigest("unidentified:" + source)
				}
				// An interrupted retry grants no new execution evidence.
				// Keep its unknown outcome as a distinct unresolved fact so
				// it cannot overwrite the original command's actual failure.
				fingerprint := info.fingerprint
				if m.Kind == "interrupted" {
					fingerprint = factDigest("unknown:" + info.fingerprint)
				}
				d.Failures = append(d.Failures, checkpoint.Failure{Fingerprint: fingerprint, Source: source, Tool: info.name, Command: info.command, Cwd: info.cwd, Output: excerpt})
			} else {
				backgroundLaunch := m.Role == "tool" && m.Observation != nil && len(m.Observation.Jobs) > 0
				if info.fingerprint != "" && !backgroundLaunch {
					d.ResolvedFailures = append(d.ResolvedFailures, info.fingerprint, factDigest("unknown:"+info.fingerprint))
				}
				if info.name == "ask_user" {
					answer := m.Content
					if m.Observation != nil && m.Observation.Answer != nil {
						answer = *m.Observation.Answer
					}
					if strings.TrimSpace(answer) != "" {
						d.Decisions = append(d.Decisions, checkpoint.Text{Source: source, Content: answer})
					}
				}
			}
		}
		state, err = checkpoint.MergeWithEviction(state, d)
		if err != nil {
			return
		}
	}
	var jobs []checkpoint.Job
	if c.Exec != nil {
		for _, job := range c.Exec.Jobs() {
			current := snapshotJob(job, false)
			for _, old := range state.Jobs {
				// A durable delivery can precede MarkDelivered by a few
				// instructions. Never retract it for the same incarnation.
				if old.ID == current.ID && old.Started.Equal(current.Started) && current.Status == checkpoint.Done {
					current.Delivered = current.Delivered || old.Delivered
				}
			}
			jobs = append(jobs, current)
		}
	}
	state, err = checkpoint.MergeWithEviction(state, checkpoint.Delta{Jobs: jobs, RefreshJobs: true})
	return
}

func checkpointMessage(state checkpoint.State) (llm.Message, error) {
	data, err := checkpoint.Encode(state)
	return llm.Message{Role: "user", Kind: "checkpoint", Content: string(data)}, err
}

func hasCheckpointFacts(s checkpoint.State) bool {
	return s.ActiveTask != nil || len(s.Constraints)+len(s.Decisions)+len(s.Verification)+len(s.Failures)+len(s.Pending)+len(s.Jobs)+len(s.Artifacts) > 0
}
