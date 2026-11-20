package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

type stoppedJobTransport func(*http.Request) (*http.Response, error)

func (f stoppedJobTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func runStoppedJobSteps(t *testing.T, c *Config, steps ...func() string) ([]string, error) {
	t.Helper()
	var req []string
	c.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: stoppedJobTransport(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		req = append(req, string(b))
		s := textSSE("done")
		if len(req) <= len(steps) {
			s = steps[len(req)-1]()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(s))}, nil
	})}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := c.Run(ctx, []llm.Message{{Role: "user", Content: "work"}})
	return req, err
}
func stoppedJobSession(t *testing.T) (*Config, *session.Store) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	t.Setenv("HOME", t.TempDir())
	c := budgetConfig(t, nil)
	s, err := session.Open("stopped-job-regressions", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); c.Exec.CancelJobs() })
	c.Session = s
	return c, s
}
func waitStoppedJobFile(t *testing.T, c *Config, path string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(c.Exec.Root, path)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing %s", path)
		}
		time.Sleep(time.Millisecond)
	}
}
func stoppedJobLaunch(command string) string {
	b, _ := json.Marshal(map[string]any{"command": command, "background": true})
	return string(b)
}

const stoppedJobStop = `{"killJobId":"bg-1"}`

func TestStoppedJobRepeatedStopDoesNotRepeatOutput(t *testing.T) {
	c, _ := stoppedJobSession(t)
	args := stoppedJobLaunch(`printf 'stopped-%s' original; : > ready; exec sleep 30`)
	done := 0
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolBackgroundDone {
			done++
		}
	}
	requests, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return budgetSSE("", []llm.ToolCall{budgetCall("stop-a", "bash", stoppedJobStop), budgetCall("stop-b", "bash", stoppedJobStop)}, nil, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if done != 1 || strings.Count(requests[2], "stopped-original") != 1 {
		t.Fatalf("completion events=%d, model body occurrences=%d", done, strings.Count(requests[2], "stopped-original"))
	}
}

func TestStoppedJobSaveFailureStopsBeforeGrace(t *testing.T) {
	c, s := stoppedJobSession(t)
	c.MaxSteps = 2
	args := stoppedJobLaunch(`printf 'stopped-%s' original; : > ready; exec sleep 30`)
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolStart && e.ToolCallID == "stop" {
			s.Close()
		}
	}
	requests, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("stop", "bash", stoppedJobStop)
	})
	if !errors.Is(err, os.ErrClosed) || c.Exec.Jobs()[0].Delivered() {
		t.Fatalf("expected save error and an undelivered job, got %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("failed save made %d provider requests, want 2 without grace", len(requests))
	}
}

func TestStoppedJobBinaryOutputRetainsFailure(t *testing.T) {
	c, s := stoppedJobSession(t)
	args := stoppedJobLaunch(`printf '\377stopped-%s' original; : > ready; exec sleep 30`)
	_, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("stop", "bash", stoppedJobStop)
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := (&Config{}).compactState(history)
	if err != nil {
		t.Fatal(err)
	}
	key := factDigest(CanonicalToolKey("bash", args))
	if !slices.ContainsFunc(state.Failures, func(f checkpoint.Failure) bool {
		return f.Fingerprint == key && strings.Contains(f.Output.Head, "stopped-original")
	}) {
		t.Fatalf("missing original failure: %+v", state.Failures)
	}
}

func TestStoppedJobResultHookCannotHideFailureOutput(t *testing.T) {
	c, _ := stoppedJobSession(t)
	args := stoppedJobLaunch(`printf 'important-%s\n' beginning; printf '%010000d\n' 0; printf 'stopped-%s' tail; : > ready; exec sleep 30`)
	c.Exec.Plugins = &fakePluginRunner{result: map[string]string{"bash": strings.Repeat("hook decoration\n", 2400)}}
	requests, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "ready")
		return toolCallSSE("stop", "bash", stoppedJobStop)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"important-beginning", "Background job bg-1 finished with error", "stopped-tail"} {
		if !strings.Contains(requests[2], want) {
			t.Errorf("stop hook crowded out %q", want)
		}
	}
}

func TestStoppedJobPreservesEarlierSiblingOrder(t *testing.T) {
	c, s := stoppedJobSession(t)
	args := stoppedJobLaunch(`if mkdir first 2>/dev/null; then printf 'newer-%s\n' head; printf '%040000d' 0; printf 'newer-%s\n' tail; exec sleep 30; fi; : > started; while ! test -f release; do sleep .01; done; printf 'earlier-%s\n' head; printf '%040000d' 0; printf 'earlier-%s\n' tail`)
	c.Exec.Audit = func(e tools.AuditEntry) {
		if e.Kind == "bash" && e.ToolCallID == "launch-first" {
			if err := os.WriteFile(filepath.Join(c.Exec.Root, "release"), nil, 0600); err != nil {
				t.Error(err)
				return
			}
			jobs := c.Exec.Jobs()
			if len(jobs) != 2 {
				t.Errorf("jobs after stop: %d", len(jobs))
				return
			}
			select {
			case <-jobs[1].Done():
			case <-time.After(4 * time.Second):
				t.Error("sibling did not finish")
			}
		}
	}
	var doneIDs []string
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolBackgroundDone {
			doneIDs = append(doneIDs, e.JobID)
		}
	}
	requests, err := runStoppedJobSteps(t, c, func() string { return toolCallSSE("launch-first", "bash", args) }, func() string {
		waitStoppedJobFile(t, c, "first")
		return toolCallSSE("launch-second", "bash", args)
	}, func() string {
		waitStoppedJobFile(t, c, "started")
		return toolCallSSE("stop", "bash", stoppedJobStop)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"earlier-head", "earlier-tail", "newer-head", "newer-tail"} {
		if !strings.Contains(requests[len(requests)-1], want) {
			t.Errorf("batched delivery lost %q", want)
		}
	}
	history, err := session.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, _, _, err := (&Config{}).compactState(history)
	if err != nil {
		t.Fatal(err)
	}
	key := factDigest(CanonicalToolKey("bash", args))
	if !slices.ContainsFunc(state.Failures, func(f checkpoint.Failure) bool { return f.Fingerprint == key }) {
		t.Fatalf("latest stop failure incorrectly cleared by earlier success (events: %v)", doneIDs)
	}
}
