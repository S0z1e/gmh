package agent

import (
	"context"
	"minimalharness/internal/tools"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/session"
)

func TestQuotaAdmissionPreservesActualCommandFailure(t *testing.T) {
	for _, mode := range []string{"direct", "foreground-job", "background"} {
		t.Run(mode, func(t *testing.T) {
			c, s := stoppedJobSession(t)
			if mode == "foreground-job" {
				c.Exec.BackgroundThreshold = 200 * time.Millisecond
			}
			c.Exec.OutputDir = t.TempDir()
			c.Exec.OutputMaxBytes = int64(len("actual-command-diagnostic"))
			args := `{"command":"printf actual-command-diagnostic; exit 2"}`
			if mode == "background" {
				args = stoppedJobLaunch("printf actual-command-diagnostic; exit 2")
			}
			_, err := runStoppedJobSteps(t, c,
				func() string { return toolCallSSE("first", "bash", args) },
				func() string {
					if mode == "background" {
						<-c.Exec.Jobs()[0].Done()
					}
					return toolCallSSE("quota-rejected", "bash", args)
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			h, err := session.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			denied := false
			for _, m := range h {
				if m.Role == "tool" && m.ToolCallID == "quota-rejected" {
					if !strings.Contains(m.Content, "session bash output reached") {
						t.Fatalf("not quota rejected: %+v", m)
					}
					if m.Kind != notExecutedKind || m.Observation == nil || m.Observation.Failure != nil {
						t.Fatalf("refusal persisted as execution: %+v", m)
					}
					denied = true
				}
			}
			if !denied {
				t.Fatal("missing quota rejection")
			}
			state, _, _, err := c.compactState(h)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range state.Failures {
				if f.Fingerprint == factDigest(CanonicalToolKey("bash", args)) {
					if !strings.Contains(f.Output.Head, "actual-command-diagnostic") {
						t.Fatalf("admission rejection overwrote actual command failure: %+v", f)
					}
					return
				}
			}
			t.Fatal("missing original failure")
		})
	}
}

func TestQuotaNoticeSurvivesStructuredDisplay(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = t.TempDir()
	c.Exec.OutputMaxBytes = 6
	job, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "printf 1234567890", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	res, _ := job.Result()
	original, _ := jobResultMessage(job, res)
	m := completedJobMessage(job, res, original+strings.Repeat("long-progress-note", 100))
	got := completedJobDisplay(m, 512)
	if len(got) > 512 || !strings.Contains(got, "session bash output reached the 6-byte limit") || !strings.Contains(got, "command stopped") {
		t.Fatalf("lost quota notice: %q", got)
	}
}
