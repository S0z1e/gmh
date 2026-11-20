// Package eval runs fixed, oracle-backed mechanism scenarios. It measures the
// harness with scripted providers; it does not estimate real-model quality.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Scenario struct{ ID, Package, Test, Oracle string }

var Scenarios = []Scenario{
	{"small-change", "./internal/agent", "TestTaskSmallChange", "exact file bytes, unchanged file and directory scope; required check"},
	{"cross-file-refactor", "./internal/agent", "TestTaskCrossFileRefactor", "both references updated; unchanged file and directory scope; required check"},
	{"fail-edit-rerun", "./internal/agent", "TestFailureProgressRerunsRepairedCommandAfterThreeFailures", "four actual command executions, final repaired check"},
	{"nested-guidance", "./internal/agent", "TestGuidanceFollowsEffectiveTargetAndDefersBatch", "guidance on effective target precedes mutation; exact output file"},
	{"long-output", "./internal/tools", "TestBashForegroundOutputFile", "full capture exists and bounded display references it"},
	{"repeated-compaction", "./internal/agent", "TestStructuredCheckpointRepeatedResumeAndRollback", "two durable checkpoints preserve constraints, failure, image, jobs and archives"},
	{"image-update", "./internal/app", "TestImageHistoryDoesNotReloadItsOrigin", "historical bytes stay fixed after origin replacement"},
	{"background-build-failure", "./internal/agent", "TestFailureProgressBackgroundFailureUsesSameGuard", "three actual failures delivered; repeated command blocked"},
	{"cancel-resume", "./internal/agent", "TestStoppedBackgroundJobSurvivesAbortAndResolvesBeforeRetry", "abort preserves durable failed outcome and resumed repair state"},
	{"document-injection", "./internal/agent", "TestTaskDocumentInjectionKeepsAuthority", "tool payload remains data; keep file unchanged; required check"},
}

type Result struct {
	Model            string   `json:"resolved_model,omitempty"`
	ToolCalls        int      `json:"tool_calls,omitempty"`
	ToolFailures     int      `json:"tool_failures,omitempty"`
	Compactions      int      `json:"compactions,omitempty"`
	ClaimedCompleted bool     `json:"claimed_completed,omitempty"`
	FalseCompletion  bool     `json:"false_completion,omitempty"`
	ScopeViolations  []string `json:"scope_violations,omitempty"`
	ID               string   `json:"id"`
	Oracle           string   `json:"oracle"`
	Passed           bool     `json:"passed"`
	Seconds          float64  `json:"seconds"`
	Error            string   `json:"error,omitempty"`
	// Null means no real-model measurement was made, not zero usage/cost.
	ProviderTokens *int64   `json:"provider_tokens"`
	CostUSD        *float64 `json:"cost_usd"`
}
type Report struct {
	Version       int       `json:"version"`
	HarnessCommit string    `json:"harness_commit"`
	Dirty         bool      `json:"dirty"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	Started       time.Time `json:"started"`
	Results       []Result  `json:"results"`
	SuccessRate   float64   `json:"scenario_success_rate"`
	P50           float64   `json:"p50_scenario_seconds"`
	P95           float64   `json:"p95_scenario_seconds"`
}

type testEvent struct {
	Action, Test string
	Elapsed      float64
	Output       string
}

// parse validates that the exact named test really ran and passed. Empty test
// selections, skips, malformed/truncated output and package failures fail closed.
func parse(r io.Reader, name string) (float64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 2<<20)
	ran, passed, packagePass := false, false, false
	elapsed := 0.0
	var failure string
	for sc.Scan() {
		var e testEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return 0, fmt.Errorf("invalid go test event: %w", err)
		}
		if e.Test == name {
			switch e.Action {
			case "run":
				ran = true
			case "pass":
				passed = true
				elapsed = e.Elapsed
			case "fail", "skip":
				elapsed = e.Elapsed
				failure = e.Action
			}
		}
		if e.Test == "" {
			if e.Action == "fail" {
				failure = "package failed"
			}
			if e.Action == "pass" {
				packagePass = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if !ran || !passed || !packagePass || failure != "" {
		return elapsed, fmt.Errorf("test %s: ran=%t passed=%t package_pass=%t %s", name, ran, passed, packagePass, failure)
	}
	return elapsed, nil
}

func Run(ctx context.Context, root string) (Report, error) {
	report := Report{Version: 1, Provider: "scripted-fixtures", Model: "scripted-v1", Started: time.Now().UTC()}
	report.HarnessCommit = gitOutput(ctx, root, "rev-parse", "HEAD")
	report.Dirty = gitOutput(ctx, root, "status", "--porcelain", "--untracked-files=normal") != ""
	failed := false
	var durations []float64
	for _, scenario := range Scenarios {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result := Result{ID: scenario.ID, Oracle: scenario.Oracle}
		taskCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		f, err := os.CreateTemp("", "harness-eval-*.jsonl")
		if err != nil {
			cancel()
			return report, err
		}
		cmd := exec.CommandContext(taskCtx, "go", "test", "-json", "-count=1", "-timeout=90s", scenario.Package, "-run", "^"+regexp.QuoteMeta(scenario.Test)+"$")
		cmd.Dir = root
		cmd.Stdout = f
		cmd.Stderr = os.Stderr
		runErr := cmd.Run()
		cancel()
		_, seekErr := f.Seek(0, 0)
		if seekErr == nil {
			result.Seconds, err = parse(f, scenario.Test)
		} else {
			err = seekErr
		}
		f.Close()
		os.Remove(f.Name())
		if runErr != nil {
			err = fmt.Errorf("go test: %w (%v)", runErr, err)
		}
		result.Passed = err == nil
		if err != nil {
			result.Error = err.Error()
			failed = true
		}
		report.Results = append(report.Results, result)
		durations = append(durations, result.Seconds)
	}
	successes := 0
	for _, r := range report.Results {
		if r.Passed {
			successes++
		}
	}
	report.SuccessRate = float64(successes) / float64(len(report.Results))
	sort.Float64s(durations)
	report.P50 = durations[(len(durations)-1)/2]
	report.P95 = durations[len(durations)-1]
	if failed {
		return report, fmt.Errorf("one or more fixed scenarios failed")
	}
	return report, nil
}

func gitOutput(ctx context.Context, root string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}
