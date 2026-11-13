package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

// Background results report in at the next step boundary, not only
// when the turn ends: a background:true job started in step 1 is
// delivered to the model in step 3's request, while the turn is
// still going (the model keeps calling tools afterwards).
func TestRunBackgroundJobDeliveredAtNextStepBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "read.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolCall := func(id, name string, args map[string]any) string {
		ab, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%s,\"function\":{\"name\":%s,\"arguments\":%s}}]}}]}\n\n",
			strconv.Quote(id), strconv.Quote(name), strconv.Quote(string(ab)))
	}
	textReply := func(text string) string {
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", strconv.Quote(text))
	}
	usage := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"

	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			// Start a background job that sleeps 1s.
			fmt.Fprint(w, toolCall("call_1", "bash", map[string]any{
				"command": "sleep 1; echo BG-DONE", "background": true,
			}))
		case 2:
			// Stall the reply so the job finishes mid-step.
			time.Sleep(2500 * time.Millisecond)
			fmt.Fprint(w, toolCall("call_2", "read", map[string]any{
				"path": "read.txt",
			}))
		default:
			fmt.Fprint(w, textReply("all done"))
		}
		fmt.Fprint(w, usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root, BackgroundThreshold: tools.DefaultBackgroundThreshold},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 10,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "start a background job, then read read.txt"}})
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if text != "all done" {
		t.Fatalf("final text = %q, want %q", text, "all done")
	}
	if len(bodies) < 3 {
		t.Fatalf("saw %d LLM calls, want at least 3", len(bodies))
	}
	// The step-3 request must already carry the finished job's
	// result: it was delivered at the step boundary, not held
	// until the turn ended (drainPending only runs after the
	// model stops calling tools).
	if !strings.Contains(bodies[2], "Background job bg-1 finished") || !strings.Contains(bodies[2], "BG-DONE") {
		t.Fatalf("step-3 request lacks the background result; bodies:\n%s", strings.Join(bodies, "\n---\n"))
	}
}

// background:true returns at once with a job id, and the result
// arrives through collectFinishedJobs at the next boundary — while
// a foreground call still returns its result directly.
func TestExecBashBackgroundFlag(t *testing.T) {
	e := &tools.Executor{Root: t.TempDir(), BackgroundThreshold: tools.DefaultBackgroundThreshold}
	cfg := &Config{Exec: e}
	ctx := context.Background()

	bashCall := func(args string) llm.ToolCall {
		return llm.ToolCall{ID: "call_1", Type: "function", Function: llm.FunctionCall{Name: "bash", Arguments: args}}
	}

	// Foreground: result directly, no job.
	obs, job := cfg.execBash(ctx, bashCall(`{"command":"echo hi"}`))
	if job != nil {
		t.Fatalf("foreground call returned a job %+v", job)
	}
	if !strings.Contains(obs, "hi") {
		t.Fatalf("foreground observation = %q, want it to contain hi", obs)
	}

	// Explicit background: returns at once with a job.
	start := time.Now()
	obs, job = cfg.execBash(ctx, bashCall(`{"command":"sleep 5; echo BG","background":true}`))
	if time.Since(start) > 2*time.Second {
		t.Fatalf("background call took %v, want it to return at once", time.Since(start))
	}
	if job == nil {
		t.Fatal("background call returned no job")
	}
	if !strings.Contains(obs, job.ID) {
		t.Fatalf("background observation = %q, want it to name job %s", obs, job.ID)
	}
	// Nothing to collect while it runs.
	if msgs, _ := cfg.collectFinishedJobs(nil, []pendingJob{{callID: "call_1", job: job}}); len(msgs) != 0 {
		t.Fatalf("collectFinishedJobs returned %d messages for a running job", len(msgs))
	}
	<-job.Done()
	msgs, pending := cfg.collectFinishedJobs(nil, []pendingJob{{callID: "call_1", job: job}})
	if len(pending) != 0 {
		t.Fatalf("pending after delivery = %d, want 0", len(pending))
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "Background job "+job.ID+" finished") || !strings.Contains(msgs[0].Content, "BG") {
		t.Fatalf("delivered messages = %+v, want the finished job result", msgs)
	}
}

// drainPending delivers in completion order: a fast job submitted
// after a slow one reports first instead of waiting behind it.
func TestDrainPendingDeliversInCompletionOrder(t *testing.T) {
	e := &tools.Executor{Root: t.TempDir()}
	ctx := context.Background()
	slow, err := e.BashBackground(ctx, tools.BashArgs{Command: "sleep 2; echo SLOW"})
	if err != nil {
		t.Fatal(err)
	}
	fast, err := e.BashBackground(ctx, tools.BashArgs{Command: "echo FAST"})
	if err != nil {
		t.Fatal(err)
	}
	// Let the fast job finish before draining.
	time.Sleep(300 * time.Millisecond)

	cfg := &Config{Exec: e}
	msgs, err := cfg.drainPending(ctx, nil, []pendingJob{
		{callID: "c1", job: slow},
		{callID: "c2", job: fast},
	})
	if err != nil {
		t.Fatalf("drainPending error = %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("drainPending returned %d messages, want 2", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "FAST") {
		t.Fatalf("first delivered message = %q, want the fast job first", msgs[0].Content)
	}
	if !strings.Contains(msgs[1].Content, "SLOW") {
		t.Fatalf("second delivered message = %q, want the slow job second", msgs[1].Content)
	}
}

// A never-ending background job must not block turn end: the drain
// gives up after its bound and the turn ends with the job still
// running, its output path reported so the model can read it.
// (The café deadlock: a dev server started as a background job
// blocked the turn forever because drainPending waited without
// a bound.)
func TestDrainPendingEndsWithNeverEndingJob(t *testing.T) {
	root := t.TempDir()
	outDir := t.TempDir()
	toolCall := func(id, name string, args map[string]any) string {
		ab, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%s,\"function\":{\"name\":%s,\"arguments\":%s}}]}}]}\n\n",
			strconv.Quote(id), strconv.Quote(name), strconv.Quote(string(ab)))
	}
	textReply := func(text string) string {
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", strconv.Quote(text))
	}
	usage := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"

	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			fmt.Fprint(w, toolCall("call_1", "bash", map[string]any{
				"command": "sleep 120", "background": true,
			}))
		case 2:
			fmt.Fprint(w, textReply("wrapping up"))
		default:
			fmt.Fprint(w, textReply("all done"))
		}
		fmt.Fprint(w, usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client:       &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:         &tools.Executor{Root: root, OutputDir: outDir, BackgroundThreshold: tools.DefaultBackgroundThreshold},
		Tools:        ToolDefs(),
		System:       "test system",
		Mode:         "auto",
		MaxSteps:     10,
		DrainTimeout: 200 * time.Millisecond,
	}
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "start a server in the background, then finish"}})
		done <- result{text, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Run error = %v", r.err)
		}
		if r.text != "all done" {
			t.Fatalf("final text = %q, want %q", r.text, "all done")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned: the drain waited forever on the never-ending job")
	}
	if len(bodies) < 3 {
		t.Fatalf("saw %d LLM calls, want at least 3", len(bodies))
	}
	// The third request must already carry the still-running notice
	// with the job's output file path: the turn ended with the job
	// running instead of waiting for it.
	if !strings.Contains(bodies[2], "still running") || !strings.Contains(bodies[2], outDir) {
		t.Fatalf("step-3 request lacks the still-running notice with the output path; bodies:\n%s", strings.Join(bodies, "\n---\n"))
	}
}
