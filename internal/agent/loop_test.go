package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

// The loop must emit its event stream in order for a tool-call turn:
// text delta, per-step usage, tool start/end, final text, done.
func TestRunEmitsEventsInOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Reading \"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"a.txt\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	text, total, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "read a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want %q", text, "done")
	}
	if total.TotalTokens != 38 {
		t.Fatalf("total tokens = %d, want 38", total.TotalTokens)
	}
	wantKinds := []EventKind{
		EventTextDelta, EventStepUsage, EventToolStart, EventToolEnd,
		EventTextDelta, EventStepUsage, EventDone,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantKinds), events)
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Fatalf("event %d kind = %s, want %s (all: %+v)", i, events[i].Kind, want, events)
		}
	}
	if events[0].Text != "Reading " {
		t.Fatalf("event 0 text = %q, want %q", events[0].Text, "Reading ")
	}
	if events[2].ToolName != "read" || events[2].ToolArgs != `{"path":"a.txt"}` {
		t.Fatalf("tool start = %s %s", events[2].ToolName, events[2].ToolArgs)
	}
	if events[3].ToolOutput != "1\thello\n2\t\n" {
		t.Fatalf("tool end output = %q, want numbered read output", events[3].ToolOutput)
	}
	if events[6].Text != "done" || events[6].Usage.TotalTokens != 38 {
		t.Fatalf("done event = %+v", events[6])
	}
}

// The model-request trigger: when the model calls load_skill, the skill
// body reaches it as a tool result on the next call.
func TestRunLoadSkillTool(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".agents", "skills", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo\ndescription: A demo skill.\n---\n# Demo\nMARKER-BODY-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"load_skill\",\"arguments\":\"{\\\"name\\\":\\\"demo\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondBody = string(body)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "do the demo thing"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want %q", text, "done")
	}
	if !strings.Contains(secondBody, "MARKER-BODY-123") {
		t.Fatalf("second request did not contain the skill body: %s", secondBody)
	}
}

// The model can load one reference file on demand; the reference
// content reaches it as a tool result on the next call.
func TestRunLoadSkillReference(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".agents", "skills", "demo")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo\ndescription: A demo skill.\n---\n# Demo\nSee references/api.md.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "api.md"), []byte("REF-BODY-789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"load_skill\",\"arguments\":\"{\\\"name\\\":\\\"demo\\\",\\\"reference\\\":\\\"api.md\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondBody = string(body)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "do the demo thing"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "done" {
		t.Fatalf("final text = %q, want %q", text, "done")
	}
	if !strings.Contains(secondBody, "REF-BODY-789") {
		t.Fatalf("second request did not contain the reference content: %s", secondBody)
	}
}

// DenyInsteadOfPrompt refuses a write without asking anyone, the turn
// continues, and the denial is audited to the session sidecar.
func TestRunDenyInsteadOfPrompt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	store, err := session.New("deny-test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"nope.txt\\\",\\\"content\\\":\\\"x\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "User denied this tool call") {
			t.Errorf("second request should carry the denial observation: %s", body)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"understood, I will not write it\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client:              &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:                &tools.Executor{Root: root},
		Tools:               ToolDefs(),
		System:              "test system",
		Mode:                "suggest",
		MaxSteps:            5,
		Session:             store,
		DenyInsteadOfPrompt: true,
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "write nope.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "understood, I will not write it" {
		t.Fatalf("final text = %q", text)
	}
	if _, err := os.Stat(filepath.Join(root, "nope.txt")); !os.IsNotExist(err) {
		t.Fatal("denied write must not create the file")
	}
	data, err := os.ReadFile(store.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"decision":"denied"`) || !strings.Contains(string(data), `"tool":"write"`) {
		t.Fatalf("audit sidecar = %s", data)
	}
}

// A bash call that runs past the threshold backgrounds: the model gets
// a "running in background" observation, keeps working, and the result
// is fed back as a tool message with the original call id when the turn
// would otherwise end.
func TestRunBackgroundsSlowBash(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			// A bash call that sleeps past the 300ms threshold.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"sleep 2; echo SLEPT-OK\\\"}\"}}]}}]}\n\n")
		case 2:
			// Other work meanwhile: read a.txt.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_2\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]}}]}\n\n")
		case 3:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"waiting\"}}]}\n\n")
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"all done\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root, BackgroundThreshold: 300 * time.Millisecond},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 10,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "run the slow command and read a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "all done" {
		t.Fatalf("final text = %q, want %q", text, "all done")
	}
	// Call 2 must carry the backgrounded observation for call_1.
	if len(bodies) < 2 || !strings.Contains(bodies[1], "still running in the background") {
		t.Fatalf("second request should carry the backgrounded observation: %s", bodies[1])
	}
	// Call 4 (after the drain) must carry the fed-back result as a
	// user message — and the tool_call_id call_1 must appear exactly
	// once in the request: a second tool message with the same id is
	// a 400 from the API (verified live, P2-8).
	if len(bodies) < 4 {
		t.Fatalf("want at least 4 requests, got %d", len(bodies))
	}
	if !strings.Contains(bodies[3], "SLEPT-OK") || !strings.Contains(bodies[3], "Background job bg-1 finished") {
		t.Fatalf("fourth request should carry the fed-back result as a user message: %s", bodies[3])
	}
	if n := strings.Count(bodies[3], `"tool_call_id":"call_1"`); n != 1 {
		t.Fatalf("tool_call_id call_1 appears %d times in the fourth request, want exactly 1: %s", n, bodies[3])
	}
	var sawBg, sawWait, sawDone bool
	for _, e := range events {
		switch e.Kind {
		case EventToolBackgrounded:
			sawBg = true
			if e.JobID != "bg-1" {
				t.Fatalf("backgrounded job = %q, want bg-1", e.JobID)
			}
		case EventBackgroundWait:
			sawWait = true
		case EventToolBackgroundDone:
			sawDone = true
			if e.JobID != "bg-1" {
				t.Fatalf("background done job = %q, want bg-1", e.JobID)
			}
		}
	}
	if !sawBg || !sawWait || !sawDone {
		t.Fatalf("missing background events (bg=%v wait=%v done=%v): %+v", sawBg, sawWait, sawDone, events)
	}
}

// A bash call that finishes within the threshold never backgrounds:
// the result arrives synchronously and no background event fires.
func TestRunFastBashStaysSynchronous(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"echo fast\\\"}\"}}]}}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"got it\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir(), BackgroundThreshold: 5 * time.Second},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 5,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "run echo fast"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "got it" {
		t.Fatalf("final text = %q, want %q", text, "got it")
	}
	for _, e := range events {
		if e.Kind == EventToolBackgrounded || e.Kind == EventBackgroundWait || e.Kind == EventToolBackgroundDone {
			t.Fatalf("fast bash must not emit background events: %+v", events)
		}
	}
	// A bash call that finished within the threshold never becomes a
	// background job: /jobs stays empty.
	if got := cfg.Exec.Jobs(); len(got) != 0 {
		t.Fatalf("fast bash left %d jobs in the registry, want none", len(got))
	}
}

// A denied tool call still emits tool start/end so renderers stay in sync.
func TestRunEmitsEventsForDeniedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"x.txt\\\",\\\"content\\\":\\\"hi\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// The mock always returns the same write tool call, so after the
	// denial the loop calls the API again, denies again, and hits the
	// step cap. That error is expected; the point is the events.
	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: t.TempDir()},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "suggest",
		MaxSteps: 2,
		Ask:      func(prompt string) (bool, error) { return false, nil },
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "write x.txt"}})
	// The second API call returns the same write tool call, which is denied
	// again; the step cap then ends the run. That error is expected here.
	if err == nil {
		t.Fatal("expected step-cap error after repeated denials")
	}
	var sawStart, sawEnd, sawApproval bool
	for _, e := range events {
		if e.Kind == EventToolStart && e.ToolName == "write" {
			sawStart = true
		}
		if e.Kind == EventToolEnd && e.ToolName == "write" {
			sawEnd = true
		}
		if e.Kind == EventApproval && e.Text != "" {
			sawApproval = true
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("denied tool call missing start/end events: %+v", events)
	}
	if !sawApproval {
		t.Fatalf("denied tool call missing approval event: %+v", events)
	}
}
