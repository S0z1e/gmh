package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
)

func TestOversizedReplayEventPreservesEarlierHistory(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	s.connectionID = "conn-oversize"
	s.writeEvent(Event{Type: "turn_started", Text: "keep me"})
	huge := Event{Type: "tool_end", Output: strings.Repeat("z", maxReplayEventSize)}
	s.writeEvent(huge)
	s.writeEvent(Event{Type: "agent_settled", Text: "done"})

	out.Reset()
	s.handleQuery(Request{ID: "rep", Type: "replay", AfterSequence: 0})
	var resp struct {
		Data replayPage `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Data.Truncated || len(resp.Data.SkippedSequences) != 1 {
		t.Fatalf("oversized event was not isolated: %+v", resp.Data)
	}
	if len(resp.Data.Events) != 2 {
		t.Fatalf("earlier and later events = %d, want 2", len(resp.Data.Events))
	}
	var first, last Event
	if err := json.Unmarshal(resp.Data.Events[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Data.Events[1], &last); err != nil {
		t.Fatal(err)
	}
	if first.Type != "turn_started" || last.Type != "agent_settled" {
		t.Fatalf("history lost around oversized event: %s then %s", first.Type, last.Type)
	}
}

func TestCriticalLifecycleSurvivesDeltaBurst(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	s.connectionID = "conn-critical"
	origEvents := maxReplayEvents
	maxReplayEvents = 8
	t.Cleanup(func() { maxReplayEvents = origEvents })

	// Non-critical frames land first and fill the cap. Lifecycle events that
	// follow must evict those frames instead of the earlier lifecycle events.
	s.writeEvent(Event{Type: "turn_started", Text: "start"})
	for i := 0; i < 6; i++ {
		s.writeEvent(Event{Type: "text_delta", Text: "d"})
	}
	s.writeEvent(Event{Type: "tool_start", Tool: "read"})
	s.writeEvent(Event{Type: "tool_end", Tool: "read", Output: "ok"})
	s.writeEvent(Event{Type: "approval_request", ID: "appr-1", Tool: "write"})
	s.writeEvent(Event{Type: "agent_settled", Text: "settled"})

	out.Reset()
	s.handleQuery(Request{ID: "rep", Type: "replay", AfterSequence: 0})
	var resp struct {
		Data replayPage `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, raw := range resp.Data.Events {
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		got[ev.Type]++
	}
	if len(resp.Data.Events) > maxReplayEvents {
		t.Fatalf("replay retained %d events, cap %d", len(resp.Data.Events), maxReplayEvents)
	}
	for _, typ := range []string{"turn_started", "tool_start", "tool_end", "approval_request", "agent_settled"} {
		if got[typ] == 0 {
			t.Fatalf("missing critical %s in %+v (events=%d)", typ, got, len(resp.Data.Events))
		}
	}
}

func TestDurableReplaySurvivesProcessRestart(t *testing.T) {
	s, _ := usageRegressionServer(t, false)
	log, err := openReplayLog(s.app.Store.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.replayLog = log
	s.replayLogID = s.app.Store.ID
	s.connectionID = "conn-durable"
	s.writeEvent(Event{Type: "turn_started", Text: "before restart"})
	s.writeEvent(Event{Type: "agent_settled", Text: "persisted"})
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	s.replayLog = nil

	// A new process has a new connection id and an empty memory buffer. It
	// resumes the previous connection from the session replay file. HOME stays
	// put so both servers share ~/.minimal-harness/sessions.
	home := os.Getenv("HOME")
	next, out := usageRegressionServer(t, false)
	t.Setenv("HOME", home)
	next.app.Store = s.app.Store
	next.connectionID = "conn-new"
	reopened, err := openReplayLog(s.app.Store.ID)
	if err != nil {
		t.Fatal(err)
	}
	next.replayLog = reopened
	next.replayLogID = s.app.Store.ID
	t.Cleanup(func() { _ = reopened.close() })
	out.Reset()
	done := make(chan struct{})
	go func() {
		next.handleQuery(Request{ID: "resume", Type: "replay", AfterSequence: 0, ConnectionID: "conn-durable", ResumeSession: s.app.Store.ID})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replay of the open session log blocked on its own lock")
	}
	var resp struct {
		Type  string     `json:"type"`
		Error string     `json:"error"`
		Data  replayPage `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "response" || resp.Error != "" {
		t.Fatalf("replay after restart: type=%s err=%s", resp.Type, resp.Error)
	}
	if resp.Data.ConnectionID != "conn-durable" || resp.Data.Session != s.app.Store.ID || len(resp.Data.Events) != 2 || !resp.Data.OutputOpen {
		t.Fatalf("durable page: id=%s session=%s events=%d open=%t", resp.Data.ConnectionID, resp.Data.Session, len(resp.Data.Events), resp.Data.OutputOpen)
	}
}

func TestReplayAfterOutputFailureStaysReadable(t *testing.T) {
	s, _ := usageRegressionServer(t, false)
	s.connectionID = "conn-fail"
	w := newBlockedProtocolWriter()
	s.output = newProtocolOutput(w, func() {
		s.mu.Lock()
		s.outputFailed = true
		s.mu.Unlock()
	})
	s.writeEvent(Event{Type: "turn_started", Text: "still replayable"})
	<-w.started
	s.output.fail(errors.New("stdout closed"))
	select {
	case <-s.output.failed:
	case <-time.After(time.Second):
		t.Fatal("output failure was not recorded")
	}
	if s.output.usable() {
		t.Fatal("failed output still reports usable")
	}
	var buf bytes.Buffer
	s.out = &buf
	s.output = nil
	s.handleQuery(Request{ID: "rep", Type: "replay", AfterSequence: 0})
	var resp struct {
		Data replayPage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", buf.String(), err)
	}
	if resp.Data.OutputOpen {
		t.Fatal("replay hid the failed output stream")
	}
	if len(resp.Data.Events) != 1 {
		t.Fatalf("events after output failure: %d", len(resp.Data.Events))
	}
}

func TestDetachPendingApprovalIsNotATimeout(t *testing.T) {
	t.Setenv("HARNESS_RPC_DETACH_GRACE_SECONDS", "0")
	t.Setenv("HARNESS_APPROVAL_TIMEOUT_SECONDS", "300")
	s, _ := usageRegressionServer(t, false)
	s.app.Cfg.SetApprovalMode("suggest")
	s.approvals = map[string]pendingApproval{}
	s.workReady = sync.NewCond(&s.workMu)
	s.approvalTimeout = 300 * time.Second
	s.done = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.ask(agent.ApprovalRequest{ToolName: "write", Arguments: "{}"})
		done <- err
	}()
	deadline := time.After(time.Second)
	for {
		s.approvalsMu.Lock()
		n := len(s.approvals)
		s.approvalsMu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("approval was not pending")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	s.detach()
	select {
	case err := <-done:
		if !errors.Is(err, agent.ErrApprovalDetached) {
			t.Fatalf("detach error = %v, want ErrApprovalDetached", err)
		}
		if errors.Is(err, agent.ErrApprovalTimedOut) {
			t.Fatal("detach was reported as an approval timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("detach did not release the pending approval")
	}
}

func TestDetachFailsContinuationAndQuestionWithoutTimeout(t *testing.T) {
	t.Setenv("HARNESS_RPC_DETACH_GRACE_SECONDS", "0")
	s, _ := usageRegressionServer(t, false)
	s.workReady = sync.NewCond(&s.workMu)
	s.done = make(chan struct{})
	s.continuations = map[string]continuationWait{}
	s.questions = map[string]chan questionAnswer{}
	contDone := make(chan error, 1)
	questDone := make(chan error, 1)
	go func() {
		_, err := s.confirmContinuation(agent.ContinuationRequest{ToolName: "read", Context: context.Background()})
		contDone <- err
	}()
	go func() {
		_, err := s.askQuestion(agent.QuestionRequest{Context: context.Background()})
		questDone <- err
	}()
	deadline := time.After(time.Second)
	for {
		s.continuationsMu.Lock()
		cn := len(s.continuations)
		s.continuationsMu.Unlock()
		s.questionsMu.Lock()
		qn := len(s.questions)
		s.questionsMu.Unlock()
		if cn == 1 && qn == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("waiters not pending: continuations=%d questions=%d", cn, qn)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	s.detach()
	for _, got := range []error{<-contDone, <-questDone} {
		if !errors.Is(got, agent.ErrApprovalDetached) || errors.Is(got, agent.ErrApprovalTimedOut) {
			t.Fatalf("detach error = %v", got)
		}
	}
}

func TestApprovalTimeoutZeroDisablesDeadline(t *testing.T) {
	s, _ := usageRegressionServer(t, false)
	s.app.Cfg.SetApprovalMode("suggest")
	s.approvals = map[string]pendingApproval{}
	s.approvalTimeout = 0
	s.done = make(chan struct{})
	released := make(chan struct{})
	go func() {
		defer close(released)
		_, _ = s.ask(agent.ApprovalRequest{ToolName: "write", Arguments: "{}"})
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-released:
		t.Fatal("explicit zero approval timeout fired")
	default:
	}
	s.failApprovals(agent.ErrApprovalDetached)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("approval waiter leaked")
	}
}

func TestDefaultApprovalTimeoutExpires(t *testing.T) {
	if os.Getenv("HARNESS_APPROVAL_TIMEOUT_SECONDS") != "" {
		t.Fatal("test environment already sets the approval timeout")
	}
	s, _ := usageRegressionServer(t, false)
	applyApprovalTimeout(s)
	if s.approvalTimeout != 300*time.Second {
		t.Fatalf("default approval timeout = %s, want 300s", s.approvalTimeout)
	}
	s.app.Cfg.SetApprovalMode("suggest")
	s.approvals = map[string]pendingApproval{}
	s.approvalTimeout = 20 * time.Millisecond
	s.done = make(chan struct{})
	_, err := s.ask(agent.ApprovalRequest{ToolName: "write", Arguments: "{}"})
	if !errors.Is(err, agent.ErrApprovalTimedOut) {
		t.Fatalf("timeout error = %v", err)
	}
	if errors.Is(err, agent.ErrApprovalDetached) {
		t.Fatal("timeout was reported as a detach")
	}
}

func TestSlowReaderTimesOutActiveTurnAfterDetachGrace(t *testing.T) {
	t.Setenv("HARNESS_RPC_DETACH_GRACE_SECONDS", "1")
	s, _ := usageRegressionServer(t, false)
	inR, inW := io.Pipe()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
		return nil, r.Context().Err()
	})
	out := newBlockingWriter()
	done := make(chan error, 1)
	go func() { done <- Run(s.app, s.runner, inR, out) }()
	select {
	case <-out.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("ready was not written")
	}
	out.mu.Lock()
	out.hold = make(chan struct{})
	out.blocked = make(chan struct{})
	out.mu.Unlock()
	if _, err := io.WriteString(inW, "{\"id\":\"p1\",\"type\":\"prompt\",\"message\":\"slow\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	select {
	case <-out.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("stdout write did not block")
	}
	select {
	case <-cancelled:
	case <-time.After(8 * time.Second):
		t.Fatal("blocked stdout did not cancel the active turn after the write timeout and detach grace")
	}
	_ = inW.Close()
	out.unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit")
	}
}

func TestReplayCapCursorTruncatesAtTheUnstoredSequence(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	orig := replayLogByteCap
	replayLogByteCap = 700
	t.Cleanup(func() { replayLogByteCap = orig })
	log, err := openReplayLog(s.app.Store.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.replayLog = log
	s.replayLogID = s.app.Store.ID
	s.connectionID = "capped"
	for range 40 {
		s.writeEvent(Event{Type: "text_delta", Text: strings.Repeat("x", 40)})
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	s.replayLog = nil
	events, _, capAt, _, err := loadReplayLog(s.app.Store.ID, "capped")
	if err != nil {
		t.Fatal(err)
	}
	if capAt == 0 || len(events) == 0 || capAt != events[len(events)-1].sequence+1 {
		t.Fatalf("cap=%d events=%d", capAt, len(events))
	}
	s.connectionID = "reader"
	read := func(after uint64) replayPage {
		t.Helper()
		out.Reset()
		s.handleQuery(Request{ID: "cap", Type: "replay", AfterSequence: after, ConnectionID: "capped", ResumeSession: s.app.Store.ID})
		var resp struct {
			Data replayPage `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Data
	}
	first := read(0)
	if first.Truncated || len(first.Events) == 0 || first.LatestSequence >= capAt {
		t.Fatalf("prefix before cap: %+v cap=%d", first, capAt)
	}
	caughtUp := read(events[len(events)-1].sequence)
	if !caughtUp.Truncated || len(caughtUp.Events) != 0 {
		t.Fatalf("cursor at last stored event: %+v", caughtUp)
	}
}

func TestReplayLogReadsLinesPastTheReadBuffer(t *testing.T) {
	s, _ := usageRegressionServer(t, false)
	log, err := openReplayLog(s.app.Store.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.replayLog = log
	s.replayLogID = s.app.Store.ID
	s.connectionID = "wide"
	wide := strings.Repeat("w", 70<<10)
	s.writeEvent(Event{Type: "tool_end", Output: wide})
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events, _, _, _, err := loadReplayLog(s.app.Store.ID, "wide")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("wide event was not replayed: %d", len(events))
	}
}

func TestTornReplayTailDoesNotSwallowTheNextEvent(t *testing.T) {
	s, _ := usageRegressionServer(t, false)
	path, err := replayLogPath(s.app.Store.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepathDir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"sequence":1,"connection_id":"c","type":"turn_started"`), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := openReplayLog(s.app.Store.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.replayLog = log
	s.replayLogID = s.app.Store.ID
	s.connectionID = "c"
	s.sequence = 1
	s.writeEvent(Event{Type: "agent_settled", Text: "after tear"})
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events, evicted, _, _, err := loadReplayLog(s.app.Store.ID, "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || evicted == 0 {
		t.Fatalf("torn tail fused with the next event: events=%d evicted=%d", len(events), evicted)
	}
	var ev Event
	if err := json.Unmarshal(events[0].data, &ev); err != nil || ev.Type != "agent_settled" {
		t.Fatalf("next event = %+v err=%v", ev, err)
	}
}

func filepathDir(path string) string {
	return filepath.Dir(path)
}

func TestReconnectAfterTransportFailureReplaysDurableEvents(t *testing.T) {
	t.Setenv("HARNESS_RPC_DETACH_GRACE_SECONDS", "3")
	s, _ := usageRegressionServer(t, false)
	home := os.Getenv("HOME")
	root := s.app.Root
	sessionID := s.app.Store.ID
	release := make(chan struct{})
	started := make(chan struct{})
	completed := make(chan struct{})
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			close(completed)
			body := "data: {\"choices\":[{\"delta\":{\"content\":\"reconnected\"}}]}\n\ndata: [DONE]\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	inR, inW := io.Pipe()
	out := newBlockingWriter()
	done := make(chan error, 1)
	go func() { done <- Run(s.app, s.runner, inR, out) }()
	select {
	case <-out.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("ready was not written")
	}
	out.mu.Lock()
	out.hold = make(chan struct{})
	out.blocked = make(chan struct{})
	out.mu.Unlock()
	if _, err := io.WriteString(inW, "{\"type\":\"prompt\",\"message\":\"survive disconnect\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	select {
	case <-out.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("stdout did not block")
	}
	time.Sleep(5500 * time.Millisecond)
	close(release)
	select {
	case <-completed:
	case <-time.After(4 * time.Second):
		t.Fatal("turn did not finish during detach grace")
	}
	_ = inW.Close()
	out.unblock()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, errOutputBackpressure) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit")
	}

	t.Setenv("HOME", home)
	store, err := session.Open(sessionID, session.OpenOptions{Workspace: root, Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	client := &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: usageRegressionTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")), Header: http.Header{}}, nil
	})}}
	nextApp := &app.App{Root: root, Store: store, Cfg: &agent.Config{Client: client, Session: store, Exec: &tools.Executor{Root: root}, Mode: "auto"}}
	nextRunner := &app.Runner{App: nextApp, Print: func(string, ...any) {}}
	in2R, in2W := io.Pipe()
	out2R, out2W := io.Pipe()
	defer in2W.Close()
	defer out2R.Close()
	nextDone := make(chan error, 1)
	go func() { nextDone <- Run(nextApp, nextRunner, in2R, out2W) }()
	reader := bufio.NewReader(out2R)
	readyLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var ready Event
	if err := json.Unmarshal(readyLine, &ready); err != nil || ready.Type != "ready" || ready.ConnectionID == "" {
		t.Fatalf("ready = %s err=%v", readyLine, err)
	}
	path, err := replayLogPath(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var head struct {
			ConnectionID string `json:"connection_id"`
			Type         string `json:"type"`
		}
		if json.Unmarshal(line, &head) == nil && head.Type == "turn_started" && head.ConnectionID != "" && head.ConnectionID != ready.ConnectionID {
			previous = head.ConnectionID
			break
		}
	}
	if previous == "" {
		t.Fatal("transport failure left no durable turn_started")
	}
	if _, err := fmt.Fprintf(in2W, "{\"id\":\"again\",\"type\":\"replay\",\"after_sequence\":0,\"connection_id\":%q,\"resume_session\":%q}\n", previous, sessionID); err != nil {
		t.Fatal(err)
	}
	var respLine []byte
	deadline := time.After(2 * time.Second)
	for respLine == nil {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(line, []byte(`"id":"again"`)) {
			respLine = line
			break
		}
		select {
		case <-deadline:
			t.Fatalf("replay response did not arrive: %s", line)
		default:
		}
	}
	var resp struct {
		Data replayPage `json:"data"`
	}
	if err := json.Unmarshal(respLine, &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, raw := range resp.Data.Events {
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		got[ev.Type]++
	}
	if got["turn_started"] == 0 || got["agent_settled"] == 0 {
		t.Fatalf("reconnect replay lost lifecycle events: %+v", got)
	}
	if !resp.Data.OutputOpen {
		t.Fatal("the new process reported its own stdout as closed")
	}
	_ = in2W.Close()
	select {
	case <-nextDone:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnected server did not exit")
	}
}

type blockingWriter struct {
	mu      sync.Mutex
	ready   chan struct{}
	blocked chan struct{}
	n       int
	hold    chan struct{}
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{ready: make(chan struct{}), blocked: make(chan struct{}), hold: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.n++
	n := w.n
	hold := w.hold
	blocked := w.blocked
	w.mu.Unlock()
	if n == 1 {
		close(w.ready)
		return len(p), nil
	}
	select {
	case <-blocked:
	default:
		close(blocked)
	}
	<-hold
	return len(p), nil
}

func (w *blockingWriter) unblock() {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.hold:
	default:
		close(w.hold)
	}
}

func (w *blockingWriter) Close() error { w.unblock(); return nil }
