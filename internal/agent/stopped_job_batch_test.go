package agent

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestStoppedJobLargeBatchKeepsVisibleResults(t *testing.T) {
	c, _ := stoppedJobSession(t)
	c.Exec.OutputDir = ""
	c.OnEvent = func(e Event) {
		if e.Kind != EventToolStart || e.ToolCallID != "stop" {
			return
		}
		for range 255 {
			job, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: `printf '%s-%s' sibling visible`, Background: true})
			if err != nil {
				t.Fatal(err)
			}
			<-job.Done()
		}
	}
	requests, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' target visible; : > ready; exec sleep 30`))
	}, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("stop", "bash", stoppedJobStop)
	})
	if err != nil {
		t.Fatal(err)
	}
	if job := c.Exec.Jobs()[len(c.Exec.Jobs())-1]; !job.Delivered() {
		t.Fatal("test expected acknowledged job")
	}
	if !strings.Contains(requests[2], "target-visible") || !strings.Contains(requests[2], "sibling-visible") {
		t.Fatalf("zero-byte result markers: %d, target visible: %t, sibling visible: %t", strings.Count(requests[2], "0 chars shown"), strings.Contains(requests[2], "target-visible"), strings.Contains(requests[2], "sibling-visible"))
	}
}

func TestStoppedJobEscapedOutputFitsSeparateRecords(t *testing.T) {
	c, store := stoppedJobSession(t)
	c.Exec.OutputDir = ""
	c.OnEvent = func(e Event) {
		if e.Kind != EventToolStart || e.ToolCallID != "stop" {
			return
		}
		for range 180 {
			job, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: `printf '%06000d' 0 | tr 0 '\000'; exit 2`, Background: true})
			if err != nil {
				t.Fatal(err)
			}
			<-job.Done()
		}
	}
	requests, err := runStoppedJobSteps(t, c, func() string {
		return toolCallSSE("launch", "bash", stoppedJobLaunch(`: > ready; exec sleep 30`))
	}, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("stop", "bash", stoppedJobStop)
	})
	if err != nil && c.LastTerminalReason != TerminalBlocked {
		t.Fatalf("stop batch failed: %v (requests %d)", err, len(requests))
	}
	saved, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, m := range saved {
		if m.Kind == "job" && m.Observation != nil {
			for _, job := range m.Observation.Jobs {
				if job.Delivered {
					delivered++
				}
			}
		}
	}
	if delivered != 181 {
		t.Fatalf("saved completions: %d, want 181", delivered)
	}
}

func TestStoppedJobRecordedCompletionDoesNotRepeat(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "separate-result"
		if legacy {
			name = "legacy-batch"
		}
		t.Run(name, func(t *testing.T) {
			c, s := stoppedJobSession(t)
			c.OnEvent = func(e Event) {
				if e.Kind != EventToolEnd || e.ToolCallID != "stop" {
					return
				}
				job := c.Exec.Jobs()[0]
				res, ok := job.Result()
				if !ok {
					t.Fatal("stopped target is not ready")
				}
				output, _ := jobResultMessage(job, res)
				// Model a complete write followed by a Sync error: the result
				// is readable, but the caller has not acknowledged delivery.
				result := llm.Message{Role: "tool", ToolCallID: e.ToolCallID, InvocationID: e.InvocationID}
				if legacy {
					result.Content = output
					result.Observation = &llm.ToolObservation{CompletedJobs: []llm.JobCompletion{jobCompletion(job, res)}}
				}
				if err := s.Append(result); err != nil {
					t.Fatal(err)
				}
				if !legacy {
					result := completedJobMessage(job, res, output)
					result.Observation.BeforeInvocationID = e.InvocationID
					if err := s.Append(result); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			_, err := runStoppedJobSteps(t, c, func() string {
				return toolCallSSE("launch", "bash", stoppedJobLaunch(`printf '%s-%s' target visible; : > ready; exec sleep 30`))
			}, func() string {
				waitStoppedJobFile(t, c, "ready")
				return toolCallSSE("stop", "bash", stoppedJobStop)
			})
			if err == nil || c.Exec.Jobs()[0].Delivered() {
				t.Fatalf("expected append failure with job unacknowledged: %v", err)
			}
			c.Session, err = session.Open(s.ID, session.OpenOptions{Workspace: c.Exec.Root})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Session.Close()
			h, err := session.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			requests, events := 0, 0
			c.OnEvent = func(e Event) {
				if e.Kind == EventToolBackgroundDone {
					events++
				}
			}
			c.Client.HTTP.Transport = stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				requests++
				if n := strings.Count(string(b), "target-visible"); n != 1 {
					t.Errorf("job output repeated %d times in first provider request", n)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(textSSE("done")))}, nil
			})
			if _, _, err := c.Run(context.Background(), h); err != nil {
				t.Fatal(err)
			}
			if requests != 1 || events != 1 {
				t.Fatalf("requests=%d completion events=%d, want one each", requests, events)
			}
		})
	}
}

func TestStoppedJobReconciliationMatchesIncarnation(t *testing.T) {
	for _, record := range []string{"result", "legacy-single", "checkpoint"} {
		for _, same := range []bool{false, true} {
			name := record + "/different-start"
			if same {
				name = record + "/same-start"
			}
			t.Run(name, func(t *testing.T) {
				c, store := stoppedJobSession(t)
				job, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "printf output"})
				if err != nil {
					t.Fatal(err)
				}
				<-job.Done()
				snapshot := snapshotJob(job, true)
				if !same {
					snapshot.Started = snapshot.Started.Add(-time.Second)
				}
				m := llm.Message{Role: "user", Kind: "job", Observation: &llm.ToolObservation{Jobs: []checkpoint.Job{snapshot}}}
				switch record {
				case "legacy-single":
					m.Observation = &llm.ToolObservation{CompletedJob: &llm.JobCompletion{Job: snapshot}}
				case "checkpoint":
					state := checkpoint.State{Version: checkpoint.Version, Jobs: []checkpoint.Job{snapshot}}
					m, err = checkpointMessage(state)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := store.Append(m); err != nil {
					t.Fatal(err)
				}
				if err := c.reconcileJobDeliveries(); err != nil {
					t.Fatal(err)
				}
				if job.Delivered() != same {
					t.Fatalf("delivered=%t, same incarnation=%t", job.Delivered(), same)
				}
				if !slices.Contains(c.Exec.Jobs(), job) {
					t.Fatal("unexpected job eviction")
				}
			})
		}
	}
}
