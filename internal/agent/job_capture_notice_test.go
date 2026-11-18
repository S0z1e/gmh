package agent

import (
	"context"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

func TestBackgroundClosureNoticeSurvivesDisplayShare(t *testing.T) {
	for _, withFile := range []bool{false, true} {
		e := &tools.Executor{Root: t.TempDir()}
		if withFile {
			e.OutputDir = t.TempDir()
		}
		for _, cmd := range []string{`(sleep .5) & printf early; exit 0`, `(sleep .5) & printf early; exit 2`} {
			job, err := e.BashBackground(context.Background(), tools.BashArgs{Command: cmd, Background: true})
			if err != nil {
				t.Fatal(err)
			}
			<-job.Done()
			res, _ := job.Result()
			display, _ := jobResultMessage(job, res)
			m := completedJobMessage(job, res, display)
			jobs := []llm.Message{m}
			shares := jobDisplayShares(jobs, 32*1024)
			got := completedJobDisplay(m, shares[0])
			t.Logf("notice=%q share=%d rendered=%q", res.Preview.Notice, shares[0], got)
			if !strings.Contains(got, "Output capture closed") {
				t.Errorf("structured result lost closure notice")
			}
		}
	}
}

func TestJobCaptureNoticeSurvivesClippedProgressNote(t *testing.T) {
	job := &tools.Job{ID: "bg-capture"}
	res := tools.JobResult{Output: strings.Repeat("output-", 500), Preview: &tools.BashOutputPreview{Head: strings.Repeat("output-", 500), Notice: "\n[Output capture closed after the shell exited.]"}}
	original, _ := jobResultMessage(job, res)
	m := completedJobMessage(job, res, original+"\n"+strings.Repeat("progress-note-", 100))
	got := completedJobDisplay(m, 512)
	if len(got) > 512 || !strings.Contains(got, "Output capture closed") {
		t.Fatalf("bytes=%d rendered=%q", len(got), got)
	}
}
