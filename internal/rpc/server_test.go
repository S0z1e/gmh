package rpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/app"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
)

// fakeStep is one scripted LLM reply, mirroring /tmp/tui-shot/
// mock_openai.py: each POST advances one step, the last repeats.
type fakeStep struct {
	kind     string // "tool" or "text"
	tool     string
	args     map[string]any
	text     string
	thinking string // reasoning_content deltas, streamed before text
	delay    time.Duration
}

func sseChunk(delta map[string]any, finish string, usage map[string]any) string {
	chunk := map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk",
		"created": 0, "model": "mock",
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, _ := json.Marshal(chunk)
	return "data: " + string(b) + "\n\n"
}

func fakeHandler(t *testing.T, steps []fakeStep) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	idx := 0
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		step := steps[idx]
		if idx < len(steps)-1 {
			idx++
		}
		mu.Unlock()
		if step.delay > 0 {
			time.Sleep(step.delay)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		var out strings.Builder
		switch step.kind {
		case "tool":
			argsJSON, _ := json.Marshal(step.args)
			out.WriteString(sseChunk(map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_mock1", "type": "function",
				"function": map[string]any{"name": step.tool, "arguments": ""},
			}}}, "", nil))
			for i := 0; i < len(argsJSON); i += 3 {
				end := i + 3
				if end > len(argsJSON) {
					end = len(argsJSON)
				}
				out.WriteString(sseChunk(map[string]any{"tool_calls": []map[string]any{{
					"index": 0, "function": map[string]any{"arguments": string(argsJSON[i:end])},
				}}}, "", nil))
			}
			out.WriteString(sseChunk(map[string]any{}, "tool_calls", usage))
		default:
			// Thinking streams first (reasoning_content deltas),
			// then the answer — the same order a real provider
			// sends them in.
			if step.thinking != "" {
				for _, word := range strings.Split(step.thinking, " ") {
					out.WriteString(sseChunk(map[string]any{"reasoning_content": word + " "}, "", nil))
				}
			}
			for _, word := range strings.Split(step.text, " ") {
				out.WriteString(sseChunk(map[string]any{"content": word + " "}, "", nil))
			}
			out.WriteString(sseChunk(map[string]any{}, "stop", usage))
		}
		out.WriteString("data: [DONE]\n\n")
		_, _ = fmt.Fprint(w, out.String())
		flusher.Flush()
	}
}

// driver is a scripted GUI: it writes requests to the server's stdin
// and reads decoded JSON lines from its stdout.
type driver struct {
	t      *testing.T
	inW    *io.PipeWriter
	events chan map[string]any
	// pending holds events a waitFor read ahead of the wait that
	// asked for them. The server's reader goroutine (responses)
	// and agent goroutine (turn events) write protocol lines
	// concurrently, so a prompt's response and its turn_started can
	// arrive in either order — and dropping the early one strands
	// the later wait for it until it times out. A wait must never
	// lose an event it did not ask for.
	pending []map[string]any
	done    chan error
}

func startDriver(t *testing.T, a *app.App, runner *app.Runner) *driver {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	d := &driver{
		t:      t,
		inW:    inW,
		events: make(chan map[string]any, 200),
		done:   make(chan error, 1),
	}
	go func() {
		d.done <- Run(a, runner, inR, outW)
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Errorf("server wrote a non-JSON line: %q (%v)", sc.Text(), err)
				continue
			}
			d.events <- rec
		}
		close(d.events)
	}()
	return d
}

func (d *driver) send(v map[string]any) {
	d.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.inW.Write(append(b, '\n')); err != nil {
		d.t.Fatalf("write to server stdin: %v", err)
	}
}

func (d *driver) sendRaw(line string) {
	d.t.Helper()
	if _, err := d.inW.Write([]byte(line + "\n")); err != nil {
		d.t.Fatalf("write to server stdin: %v", err)
	}
}

// next returns the next event, failing the test on timeout.
func (d *driver) next(timeout time.Duration) map[string]any {
	d.t.Helper()
	select {
	case rec, ok := <-d.events:
		if !ok {
			d.t.Fatal("server closed stdout")
		}
		return rec
	case <-time.After(timeout):
		d.t.Fatal("timed out waiting for an event from the server")
		return nil
	}
}

// waitFor drains events until one of the given type arrives. Events
// of other types are kept in d.pending for a later wait instead of
// dropped: the server's reader goroutine (responses) and agent
// goroutine (turn events) write concurrently, so a prompt's
// response and its turn_started arrive in either order, and
// dropping the early one strands the wait for it. The backlog is
// searched for the wanted type first, so a wait never spins on an
// event it does not want; the backlog stays FIFO, so the order
// tests assert on (first tool, first settled) is preserved.
func (d *driver) waitFor(typ string, timeout time.Duration) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for i, rec := range d.pending {
			if rec["type"] == typ {
				d.pending = append(d.pending[:i], d.pending[i+1:]...)
				return rec
			}
		}
		rest := time.Until(deadline)
		if rest <= 0 {
			d.t.Fatalf("timed out waiting for event type %q", typ)
		}
		rec := d.next(rest)
		if rec["type"] == typ {
			return rec
		}
		d.pending = append(d.pending, rec)
	}
}

func setupTestApp(t *testing.T, mode string, steps []fakeStep, writePlugins ...func(root string)) (*app.App, *app.Runner, string) {
	t.Helper()
	srv := httptest.NewServer(fakeHandler(t, steps))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "t.txt"), []byte("theta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The harness requires a configured provider: point a mock
	// provider at the fake server. Env still wins for the key.
	// The mock model declares image input so attach_image works.
	cfg := fmt.Sprintf(`{"providers": {"mock": {"baseUrl": %q, "api": "chat", "models": [{"id": "mock", "input": ["text", "image"]}, {"id": "mock2", "api": "openai-responses"}]}}}`, srv.URL+"/v1")
	if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// writePlugins runs last, before Setup: a test can add plugin
	// directories and rewrite the config (e.g. plugins.disabled).
	for _, w := range writePlugins {
		w(root)
	}
	t.Setenv("OPENAI_API_KEY", "test")
	a, err := app.Setup(app.Params{
		Root: root, Mode: mode, MaxSteps: 10,
		BackgroundThresholdSeconds: 0, DisableBackground: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Store.Close() })
	runner := &app.Runner{App: a, Print: func(string, ...any) {}}
	return a, runner, root
}

func (d *driver) quit(t *testing.T) {
	t.Helper()
	d.send(map[string]any{"id": "q1", "type": "quit"})
	resp := d.waitFor("response", 5*time.Second)
	if resp["id"] != "q1" {
		t.Fatalf("quit response id = %v, want q1", resp["id"])
	}
	select {
	case err := <-d.done:
		if err != nil {
			t.Fatalf("server returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after quit")
	}
}

// The driver must not lose an event that arrives before the wait
// asking for it: the server's reader and agent goroutines write
// concurrently, so a prompt's turn_started can precede its
// response, and the response wait must keep it for the
// turn_started wait instead of dropping it. Same-type events still
// come back in arrival order.
func TestWaitForReplaysEarlyEvents(t *testing.T) {
	d := &driver{t: t, events: make(chan map[string]any, 10), done: make(chan error, 1)}
	d.events <- map[string]any{"type": "turn_started", "text": "task"}
	d.events <- map[string]any{"type": "response", "id": "p1"}
	resp := d.waitFor("response", 5*time.Second)
	if resp["id"] != "p1" {
		t.Fatalf("response = %v, want id p1", resp)
	}
	ev := d.waitFor("turn_started", 5*time.Second)
	if ev["text"] != "task" {
		t.Fatalf("turn_started = %v, want the early event replayed", ev)
	}
	d.events <- map[string]any{"type": "tool_start", "tool": "read"}
	d.events <- map[string]any{"type": "tool_start", "tool": "write"}
	if ev := d.waitFor("tool_start", 5*time.Second); ev["tool"] != "read" {
		t.Fatalf("first tool_start = %v, want read", ev["tool"])
	}
	if ev := d.waitFor("tool_start", 5*time.Second); ev["tool"] != "write" {
		t.Fatalf("second tool_start = %v, want write", ev["tool"])
	}
}

func TestToolEndCarriesDetailAndCount(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "tool", tool: "grep", args: map[string]any{"pattern": "theta"}},
		{kind: "tool", tool: "write", args: map[string]any{"path": "u.txt", "content": "theta\n"}},
		{kind: "text", text: "Done."},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "grep theta then write it to u.txt"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("turn_started", 5*time.Second)
	if ev := d.waitFor("tool_start", 5*time.Second); ev["tool"] != "grep" {
		t.Fatalf("first tool = %v, want grep", ev["tool"])
	}
	if ev := d.waitFor("tool_end", 5*time.Second); ev["count"] != float64(1) {
		t.Fatalf("grep tool_end count = %v, want 1", ev["count"])
	}
	if ev := d.waitFor("tool_start", 5*time.Second); ev["tool"] != "write" {
		t.Fatalf("second tool = %v, want write", ev["tool"])
	}
	if ev := d.waitFor("tool_end", 5*time.Second); ev["detail"] != "theta\n" {
		t.Fatalf("write tool_end detail = %v, want the written content", ev["detail"])
	}
	if settled := d.waitFor("agent_settled", 10*time.Second); settled["error"] != nil {
		t.Fatalf("settled with error: %v", settled["error"])
	}
	d.quit(t)
}

func TestBasicTurnAuto(t *testing.T) {
	a, runner, root := setupTestApp(t, "auto", []fakeStep{
		{kind: "tool", tool: "read", args: map[string]any{"path": "t.txt"}},
		{kind: "tool", tool: "write", args: map[string]any{"path": "u.txt", "content": "theta\n"}},
		{kind: "text", text: "Done."},
	})
	d := startDriver(t, a, runner)
	ready := d.waitFor("ready", 5*time.Second)
	if ready["protocol"] != float64(1) {
		t.Fatalf("ready protocol = %v, want 1", ready["protocol"])
	}
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Read t.txt and write its content to u.txt"})
	resp := d.waitFor("response", 5*time.Second)
	if resp["id"] != "p1" || resp["type"] != "response" {
		t.Fatalf("prompt response = %v", resp)
	}
	if data, _ := resp["data"].(map[string]any); data["disposition"] != "started" {
		t.Fatalf("disposition = %v, want started", data["disposition"])
	}
	d.waitFor("turn_started", 5*time.Second)
	ev := d.waitFor("tool_start", 5*time.Second)
	if ev["tool"] != "read" {
		t.Fatalf("first tool = %v, want read", ev["tool"])
	}
	d.waitFor("tool_end", 5*time.Second)
	ev = d.waitFor("tool_start", 5*time.Second)
	if ev["tool"] != "write" {
		t.Fatalf("second tool = %v, want write", ev["tool"])
	}
	d.waitFor("tool_end", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("settled with error: %v", settled["error"])
	}
	usage, _ := settled["usage"].(map[string]any)
	if usage["total"] != float64(45) {
		t.Fatalf("settled usage total = %v, want 45 (3 steps x 15)", usage["total"])
	}
	if settled["text"] == nil || !strings.Contains(settled["text"].(string), "Done") {
		t.Fatalf("settled text = %v, want it to contain Done", settled["text"])
	}
	content, err := os.ReadFile(filepath.Join(root, "u.txt"))
	if err != nil || string(content) != "theta\n" {
		t.Fatalf("u.txt = %q, err = %v", content, err)
	}
	// get_state: one completed turn, not streaming.
	d.send(map[string]any{"id": "s1", "type": "get_state"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	if data["turns"] != float64(1) || data["streaming"] != false {
		t.Fatalf("get_state = %v, want turns 1, streaming false", data)
	}
	d.quit(t)
}

// The step-cap grace summary must reach the GUI: the loop ends a
// capped turn with an error event, and until the fix the server
// dropped the event's text, so agent_settled.text came out empty
// even though the grace call had succeeded.
func TestGraceTextReachesSettled(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "tool", tool: "read", args: map[string]any{"path": "t.txt"}},
		{kind: "text", text: "best effort summary"},
	})
	a.Cfg.MaxSteps = 1
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Read t.txt, then summarise it"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("turn_started", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["reason"] != "max_turns" {
		t.Fatalf("settled reason = %v, want max_turns", settled["reason"])
	}
	if settled["error"] == nil {
		t.Fatalf("settled error = nil, want the step-cap error")
	}
	if settled["text"] == nil || !strings.Contains(settled["text"].(string), "best effort summary") {
		t.Fatalf("settled text = %v, want it to contain the grace summary", settled["text"])
	}
	d.quit(t)
}

func TestApprovalApproveAndDeny(t *testing.T) {
	for _, tc := range []struct {
		name     string
		approved bool
	}{
		{"approve", true},
		{"deny", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, runner, root := setupTestApp(t, "suggest", []fakeStep{
				{kind: "tool", tool: "write", args: map[string]any{"path": "u.txt", "content": "hello"}},
				{kind: "text", text: "OK, noted."},
			})
			d := startDriver(t, a, runner)
			d.waitFor("ready", 5*time.Second)
			d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Write hello to u.txt"})
			d.waitFor("response", 5*time.Second)
			appr := d.waitFor("approval_request", 5*time.Second)
			if appr["tool"] != "write" {
				t.Fatalf("approval tool = %v, want write", appr["tool"])
			}
			id, _ := appr["id"].(string)
			if id == "" {
				t.Fatal("approval_request has no id")
			}
			d.send(map[string]any{"id": "a1", "type": "approval_response", "approval_id": id, "approved": tc.approved})
			resp := d.waitFor("response", 5*time.Second)
			if resp["type"] != "response" {
				t.Fatalf("approval_response reply = %v", resp)
			}
			settled := d.waitFor("agent_settled", 10*time.Second)
			if settled["error"] != nil {
				t.Fatalf("settled with error: %v", settled["error"])
			}
			_, err := os.Stat(filepath.Join(root, "u.txt"))
			if tc.approved && err != nil {
				t.Fatalf("approved write did not happen: %v", err)
			}
			if !tc.approved && err == nil {
				t.Fatal("denied write happened anyway")
			}
			d.quit(t)
		})
	}
}

func TestTwoApprovalsInOneTurn(t *testing.T) {
	a, runner, root := setupTestApp(t, "suggest", []fakeStep{
		{kind: "tool", tool: "write", args: map[string]any{"path": "u.txt", "content": "hello"}},
		{kind: "tool", tool: "bash", args: map[string]any{"command": "cat u.txt"}},
		{kind: "text", text: "All done."},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Write hello to u.txt, then show it with cat"})
	d.waitFor("response", 5*time.Second)
	for _, wantTool := range []string{"write", "bash"} {
		appr := d.waitFor("approval_request", 5*time.Second)
		if appr["tool"] != wantTool {
			t.Fatalf("approval tool = %v, want %s", appr["tool"], wantTool)
		}
		id, _ := appr["id"].(string)
		d.send(map[string]any{"id": "a-" + wantTool, "type": "approval_response", "approval_id": id, "approved": true})
		resp := d.waitFor("response", 5*time.Second)
		if resp["type"] != "response" {
			t.Fatalf("approval_response reply = %v", resp)
		}
	}
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("settled with error: %v", settled["error"])
	}
	content, err := os.ReadFile(filepath.Join(root, "u.txt"))
	if err != nil || string(content) != "hello" {
		t.Fatalf("u.txt = %q, err = %v", content, err)
	}
	d.quit(t)
}

func TestAbortAndSurvive(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "slow reply", delay: 3 * time.Second},
		{kind: "text", text: "fast reply"},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Say something slow"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("turn_started", 5*time.Second)
	// Let the LLM call get in flight first: aborting before the
	// request is even sent is a different (also valid) path, and the
	// fake's script only advances on a real POST.
	time.Sleep(500 * time.Millisecond)
	// Abort while the LLM call is still in flight.
	d.send(map[string]any{"id": "ab1", "type": "abort"})
	// 10s, not 5s: the full suite runs nine test binaries on 1.5
	// shared CPUs, and this wait once timed out at 5s under that
	// load even though the path itself is in-memory.
	resp := d.waitFor("response", 10*time.Second)
	if data, _ := resp["data"].(map[string]any); data["aborted"] != true {
		t.Fatalf("abort response = %v, want aborted true", resp)
	}
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["aborted"] != true {
		t.Fatalf("settled aborted = %v, want true", settled["aborted"])
	}
	// Cancellation is semantic, not an error: reason "abort" and
	// no error string.
	if settled["reason"] != "abort" {
		t.Fatalf("settled reason = %v, want abort", settled["reason"])
	}
	if settled["error"] != nil {
		t.Fatalf("aborted turn settled with error: %v", settled["error"])
	}
	// The server is still alive: a second turn works.
	d.send(map[string]any{"id": "p2", "type": "prompt", "message": "Say something fast"})
	d.waitFor("response", 5*time.Second)
	settled = d.waitFor("agent_settled", 10*time.Second)
	if settled["aborted"] == true || settled["error"] != nil {
		t.Fatalf("second turn settled badly: %v", settled)
	}
	if settled["reason"] != "end_turn" {
		t.Fatalf("second turn reason = %v, want end_turn", settled["reason"])
	}
	if settled["text"] == nil || !strings.Contains(settled["text"].(string), "fast") {
		t.Fatalf("second turn text = %v, want it to contain fast", settled["text"])
	}
	d.quit(t)
}

func TestNewSessionMalformedAndUnknown(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "hi"},
	})
	d := startDriver(t, a, runner)
	ready := d.waitFor("ready", 5*time.Second)
	firstSession, _ := ready["session"].(string)
	if firstSession == "" {
		t.Fatal("ready has no session")
	}
	d.sendRaw("not json")
	ev := d.waitFor("error", 5*time.Second)
	if ev["error"] == nil {
		t.Fatalf("malformed line should produce an error event: %v", ev)
	}
	d.send(map[string]any{"id": "b1", "type": "bogus"})
	resp := d.waitFor("error", 5*time.Second)
	if resp["id"] != "b1" {
		t.Fatalf("unknown type response = %v", resp)
	}
	// One turn, then a new session resets the counters.
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Say hi"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("agent_settled", 10*time.Second)
	d.send(map[string]any{"id": "n1", "type": "new_session"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	newSession, _ := data["session"].(string)
	if newSession == "" || newSession == firstSession {
		t.Fatalf("new_session = %v, want a fresh id", data)
	}
	d.send(map[string]any{"id": "s1", "type": "get_state"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["turns"] != float64(0) || data["messages"] != float64(0) {
		t.Fatalf("after new_session, get_state = %v, want turns 0, messages 0", data)
	}
	d.quit(t)
}

func TestQueuedPrompt(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "first reply", delay: 1500 * time.Millisecond},
		{kind: "text", text: "second reply"},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "First task"})
	resp := d.waitFor("response", 5*time.Second)
	if data, _ := resp["data"].(map[string]any); data["disposition"] != "started" {
		t.Fatalf("first prompt disposition = %v, want started", data["disposition"])
	}
	d.send(map[string]any{"id": "p2", "type": "prompt", "message": "Second task"})
	resp = d.waitFor("response", 5*time.Second)
	if data, _ := resp["data"].(map[string]any); data["disposition"] != "queued" {
		t.Fatalf("second prompt disposition = %v, want queued", data["disposition"])
	}
	first := d.waitFor("agent_settled", 10*time.Second)
	if first["text"] == nil || !strings.Contains(first["text"].(string), "first") {
		t.Fatalf("first settled text = %v", first["text"])
	}
	second := d.waitFor("agent_settled", 10*time.Second)
	if second["text"] == nil || !strings.Contains(second["text"].(string), "second") {
		t.Fatalf("second settled text = %v", second["text"])
	}
	d.quit(t)
}

// waitForResponse waits for the response to the request with the
// given id, stashing events of other types (or responses to other
// requests) in d.pending like waitFor does.
func (d *driver) waitForResponse(id string, timeout time.Duration) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for i, rec := range d.pending {
			if rec["type"] == "response" && rec["id"] == id {
				d.pending = append(d.pending[:i], d.pending[i+1:]...)
				return rec
			}
		}
		rest := time.Until(deadline)
		if rest <= 0 {
			d.t.Fatalf("timed out waiting for response to %q", id)
		}
		rec := d.next(rest)
		if rec["type"] == "response" && rec["id"] == id {
			return rec
		}
		d.pending = append(d.pending, rec)
	}
}

// noSettledWithin fails if an agent_settled event arrives within
// the given duration, draining (and discarding) any other events.
// The backlog is checked first: a settled stashed there by an
// earlier wait counts too.
func (d *driver) noSettledWithin(timeout time.Duration) {
	d.t.Helper()
	for _, rec := range d.pending {
		if rec["type"] == "agent_settled" {
			d.t.Fatalf("unexpected agent_settled: %v", rec["text"])
		}
	}
	deadline := time.After(timeout)
	for {
		select {
		case rec, ok := <-d.events:
			if !ok {
				return
			}
			if rec["type"] == "agent_settled" {
				d.t.Fatalf("unexpected agent_settled: %v", rec["text"])
			}
		case <-deadline:
			return
		}
	}
}

// clear_queue must drop queued prompts at once, from the reader:
// the sweep found it returned cleared:0 because it waited in the
// same FIFO behind the queued prompt. The running turn is untouched
// and a control item queued behind the prompts (here /compact)
// still runs. A fresh prompt afterwards starts at once, which
// proves the outstanding count is balanced again.
func TestClearQueueDropsQueuedPrompt(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "first reply", delay: 1500 * time.Millisecond},
		{kind: "text", text: "second reply"},
		{kind: "text", text: "third reply"},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "First task"})
	resp := d.waitFor("response", 5*time.Second)
	if data, _ := resp["data"].(map[string]any); data["disposition"] != "started" {
		t.Fatalf("first prompt disposition = %v, want started", data["disposition"])
	}
	for _, id := range []string{"p2", "p2b"} {
		d.send(map[string]any{"id": id, "type": "prompt", "message": "Queued task " + id})
		resp = d.waitFor("response", 5*time.Second)
		if data, _ := resp["data"].(map[string]any); data["disposition"] != "queued" {
			t.Fatalf("prompt %s disposition = %v, want queued", id, data["disposition"])
		}
	}
	// A control item queued behind the prompts must survive the drain.
	d.send(map[string]any{"id": "cx", "type": "prompt", "message": "/compact"})
	d.send(map[string]any{"id": "cq", "type": "clear_queue"})
	cq := d.waitForResponse("cq", 5*time.Second)
	if data, _ := cq["data"].(map[string]any); data["cleared"] != float64(2) {
		t.Fatalf("clear_queue cleared = %v, want 2", data["cleared"])
	}
	first := d.waitFor("agent_settled", 10*time.Second)
	if first["text"] == nil || !strings.Contains(first["text"].(string), "first") {
		t.Fatalf("first settled text = %v", first["text"])
	}
	cx := d.waitForResponse("cx", 10*time.Second)
	if cx["type"] != "response" {
		t.Fatalf("compact reply = %v, want a response", cx)
	}
	// The dropped prompts never settle, even though the fake steps
	// would have answered them at once.
	d.noSettledWithin(3 * time.Second)
	d.send(map[string]any{"id": "p3", "type": "prompt", "message": "Third task"})
	resp = d.waitFor("response", 5*time.Second)
	if data, _ := resp["data"].(map[string]any); data["disposition"] != "started" {
		t.Fatalf("prompt after clear disposition = %v, want started", data["disposition"])
	}
	third := d.waitFor("agent_settled", 10*time.Second)
	if third["text"] == nil || !strings.Contains(third["text"].(string), "third") {
		t.Fatalf("third settled text = %v", third["text"])
	}
	d.quit(t)
}

// clear_queue with nothing queued returns 0 and leaves the running
// turn alone.
func TestClearQueueEmptyLeavesRunningTurn(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "only reply", delay: 1500 * time.Millisecond},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Only task"})
	resp := d.waitFor("response", 5*time.Second)
	if data, _ := resp["data"].(map[string]any); data["disposition"] != "started" {
		t.Fatalf("prompt disposition = %v, want started", data["disposition"])
	}
	d.send(map[string]any{"id": "cq", "type": "clear_queue"})
	cq := d.waitForResponse("cq", 5*time.Second)
	if data, _ := cq["data"].(map[string]any); data["cleared"] != float64(0) {
		t.Fatalf("clear_queue cleared = %v, want 0", data["cleared"])
	}
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["text"] == nil || !strings.Contains(settled["text"].(string), "only") {
		t.Fatalf("settled text = %v", settled["text"])
	}
	d.quit(t)
}

// attach_image queues an image for the next turn; a missing file is
// an error response, and the image rides on the prompt after it.
func TestAttachImage(t *testing.T) {
	a, runner, root := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}})
	// Tiny valid PNG in the workspace root.
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pic.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)

	// Missing file: an error response, nothing queued.
	d.send(map[string]any{"id": "a1", "type": "attach_image", "path": "gone.png"})
	resp := d.waitFor("error", 5*time.Second)
	if resp["id"] != "a1" {
		t.Fatalf("missing-file attach = %v, want an error response for a1", resp)
	}
	if len(a.PendingImages) != 0 {
		t.Fatal("failed attach must not queue")
	}

	// Real file: attached, with its media type and size.
	d.send(map[string]any{"id": "a2", "type": "attach_image", "path": "pic.png"})
	resp = d.waitFor("response", 5*time.Second)
	if resp["type"] != "response" {
		t.Fatalf("attach = %v, want a response", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["attached"] != true || data["media_type"] != "image/png" {
		t.Fatalf("attach data = %v", data)
	}
	if len(a.PendingImages) != 1 {
		t.Fatalf("pending = %d, want 1", len(a.PendingImages))
	}

	// The next prompt consumes the attachment and settles.
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "describe the image"})
	d.waitFor("response", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("settled with error: %v", settled["error"])
	}
	if len(a.PendingImages) != 0 {
		t.Fatal("pending images should clear after the turn")
	}
	d.quit(t)
}

// set_model switches the model mid-session: the response names the
// new provider/model/api, get_state shows it at once, the "/model"
// slash form works the same, and a turn still runs afterwards.
func TestSetModel(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)

	d.send(map[string]any{"id": "m0", "type": "set_model", "model": "nope"})
	resp := d.waitFor("error", 5*time.Second)
	if resp["id"] != "m0" {
		t.Fatalf("unknown set_model = %v, want an error for m0", resp)
	}
	if a.Model.ID != "mock" {
		t.Fatal("failed set_model moved the model")
	}

	d.send(map[string]any{"id": "m1", "type": "set_model", "model": "mock2"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	if data["provider"] != "mock" || data["model"] != "mock2" || data["api"] != "responses" {
		t.Fatalf("set_model data = %v", data)
	}
	d.send(map[string]any{"id": "s1", "type": "get_state"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["model"] != "mock2" || data["api"] != "responses" {
		t.Fatalf("get_state after switch = %v", data)
	}

	// The "/model mock" slash form switches back.
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "/model mock"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["model"] != "mock" || data["api"] != "chat" {
		t.Fatalf("slash /model data = %v", data)
	}

	d.send(map[string]any{"id": "p2", "type": "prompt", "message": "Say hi"})
	d.waitFor("response", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("turn after switch settled with error: %v", settled["error"])
	}
	d.quit(t)
}

// list_sessions / rename_session / resume_session, plus the slash forms.
func TestListRenameResumeSessions(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)

	// A second session on disk, with one message.
	other, err := session.New("second")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Append(llm.Message{Role: "user", Content: "task about beta"}); err != nil {
		t.Fatal(err)
	}
	other.Close()
	current := a.Store.ID

	d.send(map[string]any{"id": "l1", "type": "list_sessions"})
	resp := d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	rows, _ := data["sessions"].([]any)
	if len(rows) != 2 {
		t.Fatalf("list_sessions = %v", data)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		m, _ := row.(map[string]any)
		seen[m["id"].(string)] = m["current"].(bool)
	}
	if seen[current] != true || seen["second"] != false {
		t.Fatalf("list_sessions current flags = %v", seen)
	}

	// Rename the other session, then resume it.
	d.send(map[string]any{"id": "r1", "type": "rename_session", "name": "second", "new": "renamed"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["renamed"] != true || data["to"] != "renamed" {
		t.Fatalf("rename_session data = %v", data)
	}
	d.send(map[string]any{"id": "r2", "type": "resume_session", "session": "renamed"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["session"] != "renamed" || data["messages"] != float64(1) {
		t.Fatalf("resume_session data = %v", data)
	}
	d.send(map[string]any{"id": "g1", "type": "get_state"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["session"] != "renamed" {
		t.Fatalf("get_state after resume = %v", data)
	}
	if len(a.History) != 1 || a.History[0].Content != "task about beta" {
		t.Fatalf("history after resume = %v", a.History)
	}

	// Errors: unknown resume, rename onto an existing id.
	d.send(map[string]any{"id": "r3", "type": "resume_session", "session": "nope"})
	if resp := d.waitFor("error", 5*time.Second); resp["id"] != "r3" {
		t.Fatalf("unknown resume = %v", resp)
	}
	d.send(map[string]any{"id": "r4", "type": "rename_session", "name": "renamed", "new": current})
	if resp := d.waitFor("error", 5*time.Second); resp["id"] != "r4" {
		t.Fatalf("rename onto existing = %v", resp)
	}

	// The "/sessions" slash form lists through a prompt.
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "/sessions"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	rows, _ = data["sessions"].([]any)
	if len(rows) != 2 {
		t.Fatalf("slash /sessions = %v", data)
	}
	d.quit(t)
}

// list_plugins returns the same information /plugins shows: a loaded
// plugin with its tools, a disabled one, and a skipped one with its
// reason — plus the "/plugins" slash form.
func TestListPlugins(t *testing.T) {
	writePlugins := func(root string) {
		write := func(name, entry, src string) {
			dir := filepath.Join(root, ".agents", "plugins", name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf(`{"name": %q, "description": "%s plugin.", "version": "0.1.0", "entry": %q}`, name, name, entry)
			if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, entry), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("echo", "echo.js", `harness.registerTool("echo", "Echo.", {type: "object", properties: {}}, () => "echo");`)
		write("off", "off.js", `harness.registerTool("offtool", "Off.", {type: "object", properties: {}}, () => "off");`)
		write("boom", "boom.js", `throw new Error("kaboom");`)
		// Disable "off" in the project config (replaces the config
		// setupTestApp wrote).
		cfg := `{"providers": {"mock": {"baseUrl": "http://127.0.0.1:1", "api": "chat", "models": [{"id": "mock", "input": ["text", "image"]}]}}, "plugins": {"disabled": ["off"]}}`
		if err := os.WriteFile(filepath.Join(root, ".harness.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}}, writePlugins)
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)

	d.send(map[string]any{"id": "p1", "type": "list_plugins"})
	resp := d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	rows, _ := data["plugins"].([]any)
	if len(rows) != 3 {
		t.Fatalf("list_plugins = %v", data)
	}
	byName := map[string]map[string]any{}
	for _, row := range rows {
		m, _ := row.(map[string]any)
		byName[m["name"].(string)] = m
	}
	echo := byName["echo"]
	if echo["state"] != "loaded" || echo["version"] != "0.1.0" || echo["description"] != "echo plugin." {
		t.Fatalf("echo row = %v", echo)
	}
	tools, _ := echo["tools"].([]any)
	if len(tools) != 1 || tools[0] != "echo" {
		t.Fatalf("echo tools = %v", tools)
	}
	if byName["off"]["state"] != "disabled" {
		t.Fatalf("off row = %v", byName["off"])
	}
	boom := byName["boom"]
	if boom["state"] != "skipped" || !strings.Contains(fmt.Sprintf("%v", boom["error"]), "kaboom") {
		t.Fatalf("boom row = %v", boom)
	}

	// The "/plugins" slash form answers the same query.
	d.send(map[string]any{"id": "p2", "type": "prompt", "message": "/plugins"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	rows, _ = data["plugins"].([]any)
	if len(rows) != 3 {
		t.Fatalf("slash /plugins = %v", data)
	}
	d.quit(t)
}

// query sends a read-only request and returns its data map.
func (d *driver) query(t *testing.T, id, typ string, extra map[string]any) map[string]any {
	t.Helper()
	req := map[string]any{"id": id, "type": typ}
	for k, v := range extra {
		req[k] = v
	}
	d.send(req)
	resp := d.waitFor("response", 5*time.Second)
	if resp["type"] != "response" {
		t.Fatalf("%s response = %v", typ, resp)
	}
	data, _ := resp["data"].(map[string]any)
	return data
}

func TestHistoryReturnsTranscript(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "forty two"},
	})
	d := startDriver(t, a, runner)
	ready := d.waitFor("ready", 5*time.Second)
	sessionA := ready["session"].(string)

	// Turn in session A.
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "What is 6 times 7?"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("agent_settled", 5*time.Second)

	// history defaults to the current session.
	data := d.query(t, "h1", "history", nil)
	if data["session"] != sessionA {
		t.Fatalf("history session = %v, want %s", data["session"], sessionA)
	}
	msgs, _ := data["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("history messages = %d, want >= 2", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || !strings.Contains(fmt.Sprintf("%v", first["content"]), "6 times 7") {
		t.Fatalf("first history message = %v", first)
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "assistant" || !strings.Contains(fmt.Sprintf("%v", last["content"]), "forty two") {
		t.Fatalf("last history message = %v", last)
	}

	// A second session, then fetch its transcript by id while back on A.
	d.send(map[string]any{"id": "n1", "type": "new_session"})
	resp := d.waitFor("response", 5*time.Second)
	sessionB := resp["data"].(map[string]any)["session"].(string)
	d.send(map[string]any{"id": "p2", "type": "prompt", "message": "What is 8 times 9?"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("agent_settled", 5*time.Second)
	d.send(map[string]any{"id": "r1", "type": "resume_session", "session": sessionA})
	d.waitFor("response", 5*time.Second)
	data = d.query(t, "h2", "history", map[string]any{"session": sessionB})
	msgs, _ = data["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("session B history messages = %d, want >= 2", len(msgs))
	}
	first, _ = msgs[0].(map[string]any)
	if first["role"] != "user" || !strings.Contains(fmt.Sprintf("%v", first["content"]), "8 times 9") {
		t.Fatalf("session B first message = %v", first)
	}

	// Unknown id and invalid id are error responses.
	d.send(map[string]any{"id": "h3", "type": "history", "session": "no-such-session"})
	resp = d.waitFor("error", 5*time.Second)
	if resp["id"] != "h3" {
		t.Fatalf("unknown-session history response = %v", resp)
	}
	d.send(map[string]any{"id": "h4", "type": "history", "session": "../x"})
	resp = d.waitFor("error", 5*time.Second)
	if resp["id"] != "h4" {
		t.Fatalf("invalid-id history response = %v", resp)
	}
	d.quit(t)
}

func TestCompactedEventManual(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "hello world"},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Say hello"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("agent_settled", 5*time.Second)

	d.send(map[string]any{"id": "c1", "type": "compact"})
	ev := d.waitFor("compacted", 5*time.Second)
	if ev["messages"] != float64(1) {
		t.Fatalf("compacted messages = %v, want 1", ev["messages"])
	}
	// The fake's usage is completion 5 for every call, and the
	// compact call's completion is the summary itself.
	if ev["summary_tokens"] != float64(5) {
		t.Fatalf("compacted summary_tokens = %v, want 5", ev["summary_tokens"])
	}
	resp := d.waitFor("response", 5*time.Second)
	if resp["id"] != "c1" || resp["type"] != "response" {
		t.Fatalf("compact response = %v", resp)
	}
	// History afterwards is the summary alone.
	data := d.query(t, "h1", "history", nil)
	msgs, _ := data["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("post-compact history = %d messages, want 1", len(msgs))
	}
	only, _ := msgs[0].(map[string]any)
	if only["role"] != "user" || !strings.Contains(fmt.Sprintf("%v", only["content"]), "Previous conversation summary:") {
		t.Fatalf("post-compact message = %v", only)
	}
	d.quit(t)
}

func TestCompactedEventAuto(t *testing.T) {
	// The previous request reached 80% of the window. The next
	// request must compact first and retain the active user task.
	tinyWindow := func(root string) {
		path := filepath.Join(root, ".harness.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		mock := cfg["providers"].(map[string]any)["mock"].(map[string]any)
		models := mock["models"].([]any)
		models[0].(map[string]any)["contextWindow"] = 10000
		models[0].(map[string]any)["maxTokens"] = 1000
		mock["models"] = models
		out, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "hello world"},
	}, tinyWindow)
	a.Cfg.LastPromptTokens = 8000
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "Say hello"})
	d.waitFor("response", 5*time.Second)
	// doCompact runs inside the turn, before agent_settled.
	ev := d.waitFor("compacted", 5*time.Second)
	if ev["messages"] != float64(2) || ev["summary_tokens"] != float64(5) {
		t.Fatalf("compacted = %v, want messages 2 summary_tokens 5", ev)
	}
	d.waitFor("agent_settled", 5*time.Second)
	d.quit(t)
}

func TestListModels(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "hi"}})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	data := d.query(t, "m1", "list_models", nil)
	provs, _ := data["providers"].([]any)
	if len(provs) != 1 {
		t.Fatalf("providers = %v", data)
	}
	prov, _ := provs[0].(map[string]any)
	if prov["name"] != "mock" || prov["api"] != "chat" {
		t.Fatalf("provider = %v", prov)
	}
	models, _ := prov["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v", prov)
	}
	byID := map[string]map[string]any{}
	for _, m := range models {
		mm, _ := m.(map[string]any)
		byID[mm["id"].(string)] = mm
	}
	mock := byID["mock"]
	if mock["api"] != "chat" || mock["context_window"] != float64(128000) || mock["max_tokens"] != float64(16384) {
		t.Fatalf("mock model = %v", mock)
	}
	input, _ := mock["input"].([]any)
	if len(input) != 2 || input[0] != "text" || input[1] != "image" {
		t.Fatalf("mock input = %v", input)
	}
	mock2 := byID["mock2"]
	if mock2["api"] != "responses" || mock2["context_window"] != float64(128000) || mock2["max_tokens"] != float64(16384) {
		t.Fatalf("mock2 model = %v", mock2)
	}
	input, _ = mock2["input"].([]any)
	if len(input) != 1 || input[0] != "text" {
		t.Fatalf("mock2 input = %v", input)
	}
	d.quit(t)
}

func TestListSkills(t *testing.T) {
	addSkill := func(root string) {
		dir := filepath.Join(root, ".agents", "skills", "demo")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\ndescription: A demo skill.\n---\n# Demo\nBody here.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "hi"}}, addSkill)
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	data := d.query(t, "s1", "list_skills", nil)
	rows, _ := data["skills"].([]any)
	if len(rows) != 1 {
		t.Fatalf("skills = %v", data)
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != "demo" || row["description"] != "A demo skill." {
		t.Fatalf("skill row = %v", row)
	}
	d.quit(t)
}

func TestGetStateHasWindow(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "hi"}})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	data := d.query(t, "g1", "get_state", nil)
	if data["context_window"] != float64(128000) {
		t.Fatalf("context_window = %v, want 128000", data["context_window"])
	}
	if data["max_tokens"] != float64(16384) {
		t.Fatalf("max_tokens = %v, want 16384", data["max_tokens"])
	}
	d.quit(t)
}

// set_tool_mode switches the tool set without a plugin, and
// delete_session removes another session's files.
func TestSetToolModeAndDeleteSession(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{{kind: "text", text: "Done."}})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)

	// A second session on disk, with one message.
	other, err := session.New("second")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Append(llm.Message{Role: "user", Content: "task about beta"}); err != nil {
		t.Fatal(err)
	}
	other.Close()
	current := a.Store.ID

	// Unknown or missing mode is an error; the mode does not move.
	d.send(map[string]any{"id": "m0", "type": "set_tool_mode", "mode": "bogus"})
	if resp := d.waitFor("error", 5*time.Second); resp["id"] != "m0" {
		t.Fatalf("bogus set_tool_mode = %v, want an error for m0", resp)
	}
	d.send(map[string]any{"id": "m0b", "type": "set_tool_mode"})
	if resp := d.waitFor("error", 5*time.Second); resp["id"] != "m0b" {
		t.Fatalf("missing-mode set_tool_mode = %v, want an error for m0b", resp)
	}
	if got := toolModeOf(t, d); got != "default" {
		t.Fatalf("tool_mode after bad switches = %q, want default", got)
	}

	// Switch to plan, run a turn, switch back.
	d.send(map[string]any{"id": "m1", "type": "set_tool_mode", "mode": "plan"})
	resp := d.waitFor("response", 5*time.Second)
	data, _ := resp["data"].(map[string]any)
	if data["tool_mode"] != "plan" {
		t.Fatalf("set_tool_mode data = %v", data)
	}
	if got := toolModeOf(t, d); got != "plan" {
		t.Fatalf("tool_mode after switch = %q, want plan", got)
	}
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "hello"})
	d.waitFor("response", 5*time.Second)
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("turn in plan mode settled with error: %v", settled["error"])
	}
	if settled["tool_mode"] != "plan" {
		t.Fatalf("agent_settled tool_mode = %v, want plan", settled["tool_mode"])
	}
	d.send(map[string]any{"id": "m2", "type": "set_tool_mode", "mode": "default"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["tool_mode"] != "default" {
		t.Fatalf("set_tool_mode back = %v", data)
	}

	// Delete the other session; it leaves the list. Unknown, empty
	// and current ids are errors.
	d.send(map[string]any{"id": "d1", "type": "delete_session", "session": "second"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	if data["deleted"] != true || data["session"] != "second" {
		t.Fatalf("delete_session data = %v", data)
	}
	d.send(map[string]any{"id": "l1", "type": "list_sessions"})
	resp = d.waitFor("response", 5*time.Second)
	data, _ = resp["data"].(map[string]any)
	rows, _ := data["sessions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list_sessions after delete = %v", data)
	}
	d.send(map[string]any{"id": "d2", "type": "delete_session", "session": "second"})
	if resp := d.waitFor("error", 5*time.Second); resp["id"] != "d2" {
		t.Fatalf("deleting an unknown session = %v, want an error for d2", resp)
	}
	d.send(map[string]any{"id": "d3", "type": "delete_session", "session": current})
	if resp := d.waitFor("error", 5*time.Second); resp["id"] != "d3" {
		t.Fatalf("deleting the current session = %v, want an error for d3", resp)
	}
	d.send(map[string]any{"id": "d4", "type": "delete_session"})
	if resp := d.waitFor("error", 5*time.Second); resp["id"] != "d4" {
		t.Fatalf("deleting with no id = %v, want an error for d4", resp)
	}
	d.quit(t)
}
