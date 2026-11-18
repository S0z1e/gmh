package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
)

const maxVerificationFeedback = 3

type verificationRun struct {
	policy            *verification.Policy
	baseline          *verification.Snapshot
	baselineErr       error
	touched           bool
	mutationAttempted bool
	latest            map[string]verification.Evidence
	feedback          int
}

type verificationAttempt struct {
	invocationID, toolCallID string
	checks                   []verification.Check
	cwd                      string
	before                   verification.Snapshot
	captureErr               error
	started                  time.Time
	foreground               bool
	cwdBound                 bool
}

// Capture before hooks/approval: even a read tool's plugin hooks can mutate files.
func (c *Config) verificationTouch(ctx context.Context) {
	v := c.verifier
	if v == nil || c.Exec == nil || v.touched {
		return
	}
	v.touched = true
	state, err := verification.Capture(ctx, c.Exec.Root, v.policy.Exclude)
	v.baseline, v.baselineErr = &state, err
}

// verificationArguments makes the checked cwd an explicit canonical argument
// before approval and durable invocation recording. A symlink alias can then
// change during capture without redirecting the approved execution.
func (c *Config) verificationArguments(name, args string) string {
	if c.verifier == nil || c.Exec == nil || name != "bash" {
		return args
	}
	var a tools.BashArgs
	if json.Unmarshal([]byte(args), &a) != nil || a.KillJobID != "" {
		return args
	}
	actual, err := (verification.Check{Name: "cwd", Command: a.Command, Cwd: a.Cwd}).Directory(c.Exec.Root)
	if err != nil {
		return args
	}
	for _, check := range c.verifier.policy.Required {
		if check.Command != a.Command {
			continue
		}
		wanted, err := check.Directory(c.Exec.Root)
		if err != nil || wanted != actual {
			continue
		}
		if a.Cwd == actual {
			return args
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(args), &fields) != nil {
			return args
		}
		fields["cwd"], _ = json.Marshal(actual)
		data, _ := json.Marshal(fields)
		return string(data)
	}
	return args
}

func (c *Config) verificationStart(ctx context.Context, name, args string) *verificationAttempt {
	v := c.verifier
	if v == nil || c.Exec == nil {
		return nil
	}
	c.verificationTouch(ctx)
	switch name {
	case "read", "grep", "glob", "load_skill", "ask_user", "webfetch", tools.GuidanceTool, tools.GitStatusTool:
	default:
		v.mutationAttempted = true
	}
	if name != "bash" {
		return nil
	}
	var a tools.BashArgs
	if json.Unmarshal([]byte(args), &a) != nil || a.KillJobID != "" {
		return nil
	}
	var checks []verification.Check
	var cwd string
	for _, check := range v.policy.Required {
		if check.Command != a.Command {
			continue
		}
		wanted, err := check.Directory(c.Exec.Root)
		if err != nil {
			continue
		}
		actual, err := (verification.Check{Name: check.Name, Command: a.Command, Cwd: a.Cwd}).Directory(c.Exec.Root)
		if err != nil || wanted != actual {
			continue
		}
		checks = append(checks, check)
		cwd = actual
	}
	if len(checks) == 0 {
		return nil
	}
	state, err := verification.Capture(ctx, c.Exec.Root, v.policy.Exclude)
	return &verificationAttempt{checks: checks, cwd: cwd, before: state, captureErr: err, started: time.Now().UTC(), foreground: !a.Background, cwdBound: a.Cwd == cwd}
}

func (c *Config) verificationFinish(ctx context.Context, a *verificationAttempt, res tools.ToolResult, runErr error) []llm.Message {
	if a == nil || c.verifier == nil {
		return nil
	}
	record := verification.Evidence{InvocationID: a.invocationID, ToolCallID: a.toolCallID, Command: a.checks[0].Command, Cwd: a.cwd, Version: a.before.Version, ExitCode: res.ExitCode, Started: a.started, Finished: time.Now().UTC()}
	state, err := verification.Capture(ctx, c.Exec.Root, c.verifier.policy.Exclude)
	switch {
	case runErr != nil:
		record.Error = runErr.Error()
	case !a.foreground || res.Status == tools.ResultBackground:
		record.Error = "required verification must run in the foreground"
	case !a.cwdBound:
		record.Error = "verification cwd was not bound in the approved invocation"
	case a.captureErr != nil:
		record.Error = "cannot capture verification inputs: " + a.captureErr.Error()
	case err != nil:
		record.Error = "cannot capture verification outputs: " + err.Error()
	case state.Version != a.before.Version:
		record.Error = "workspace changed during verification; run the check again on the final files"
	case record.ExitCode == nil:
		record.Error = "verification returned no exit code"
	case *record.ExitCode != 0:
		record.Error = fmt.Sprintf("verification exited with code %d", *record.ExitCode)
	}
	var messages []llm.Message
	for _, check := range a.checks {
		record.Check = check.Name
		c.verifier.latest[check.Name] = record
		data, _ := json.Marshal(record)
		messages = append(messages, llm.Message{Role: "user", Kind: "verification", Content: "[harness verification evidence]\n" + string(data)})
	}
	return messages
}

// verificationGate returns whether completion evidence is current, or the
// missing/failed requirements to feed back. Unchanged questions, reads and
// check-only turns make no task-level completion claim.
func (c *Config) verificationGate(ctx context.Context) (TaskOutcome, string) {
	v := c.verifier
	if v == nil || !v.touched {
		return "", ""
	}
	// Check jobs before taking the terminal snapshot. A job finishing after
	// a snapshot could otherwise make that snapshot stale before certification.
	running := c.Exec.HasRunningWorkspaceJobs()
	if running && (v.mutationAttempted || len(v.latest) > 0) {
		return "", "Background work is still running; required verification cannot certify the final workspace yet."
	}
	state, err := verification.Capture(ctx, c.Exec.Root, v.policy.Exclude)
	if err != nil {
		// Pure questions/reads make no completion claim. Unsupported inputs must
		// still fail closed after any potentially mutating dispatch or check.
		if !v.mutationAttempted && len(v.latest) == 0 && v.baselineErr != nil {
			return "", ""
		}
		return "", "Cannot verify workspace version: " + err.Error()
	}
	if v.baselineErr == nil && v.baseline != nil && v.baseline.Version == state.Version {
		return "", ""
	}
	if running {
		return "", "Background work is still running; required verification cannot certify the final workspace yet."
	}
	var missing []string
	for _, check := range v.policy.Required {
		record, ok := v.latest[check.Name]
		if !ok || !record.Current(check, state) {
			text := fmt.Sprintf("%s: run Bash command %q in cwd %q against the final files", check.Name, check.Command, check.Cwd)
			if ok && record.Error != "" {
				text += " (last attempt: " + record.Error + ")"
			}
			missing = append(missing, text)
		}
	}
	if len(missing) > 0 {
		return "", strings.Join(missing, "\n")
	}
	return OutcomeCompleted, ""
}

func verificationInstructions(policy *verification.Policy) string {
	if policy == nil || len(policy.Required) == 0 {
		return ""
	}
	data, _ := json.Marshal(policy)
	return "\n\n[harness required verification]\nFor changed work, run each required check via the normal bash tool in the foreground, with its exact command and cwd, after your final edits. Approval and normal shell policy still apply. A successful result before later edits is stale. Unchanged questions need no checks. The harness validates evidence independently of your final wording. Policy: " + string(data)
}
