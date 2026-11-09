package rpc

import (
	"bufio"
	"encoding/json"
	"fmt"
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
)

// fakeStep is one scripted LLM reply, mirroring /tmp/tui-shot/
// mock_openai.py: each POST advances one step, the last repeats.
type fakeStep struct {
	kind  string // "tool" or "text"
	tool  string
	args  map[string]any
	text  string
	delay time.Duration
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
	done   chan error
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

// waitFor drains events until one of the given type arrives.
func (d *driver) waitFor(typ string, timeout time.Duration) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		rest := time.Until(deadline)
		if rest <= 0 {
			d.t.Fatalf("timed out waiting for event type %q", typ)
		}
		rec := d.next(rest)
		if rec["type"] == typ {
			return rec
		}
	}
}

func setupTestApp(t *testing.T, mode string, steps []fakeStep) (*app.App, *app.Runner, string) {
	t.Helper()
	srv := httptest.NewServer(fakeHandler(t, steps))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test")
	t.Setenv("OPENAI_MODEL", "mock")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "t.txt"), []byte("theta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	// The server is still alive: a second turn works.
	d.send(map[string]any{"id": "p2", "type": "prompt", "message": "Say something fast"})
	d.waitFor("response", 5*time.Second)
	settled = d.waitFor("agent_settled", 10*time.Second)
	if settled["aborted"] == true || settled["error"] != nil {
		t.Fatalf("second turn settled badly: %v", settled)
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
