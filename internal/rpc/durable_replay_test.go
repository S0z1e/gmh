package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"minimalharness/internal/agent"
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
	maxReplayEvents = 4
	t.Cleanup(func() { maxReplayEvents = origEvents })

	// Non-critical frames land first and fill the cap. Lifecycle events that
	// follow must evict those frames instead of the earlier lifecycle events.
	for i := 0; i < 6; i++ {
		s.writeEvent(Event{Type: "text_delta", Text: "d"})
	}
	s.writeEvent(Event{Type: "turn_started", Text: "start"})
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
	out.Reset()
	next.handleQuery(Request{ID: "resume", Type: "replay", AfterSequence: 0, ConnectionID: "conn-durable", ResumeSession: s.app.Store.ID})
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
	if resp.Data.ConnectionID != "conn-durable" || resp.Data.Session != s.app.Store.ID || len(resp.Data.Events) != 2 {
		t.Fatalf("durable page: id=%s session=%s events=%d", resp.Data.ConnectionID, resp.Data.Session, len(resp.Data.Events))
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
	t.Setenv("HARNESS_RPC_DETACH_GRACE_SECONDS", "0")
	s, _ := usageRegressionServer(t, false)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
		return nil, r.Context().Err()
	})
	done := make(chan error, 1)
	go func() { done <- Run(s.app, s.runner, inR, outW) }()
	reader := bufio.NewReader(outR)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(inW, "{\"id\":\"p1\",\"type\":\"prompt\",\"message\":\"slow\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	outR.Close()
	_, _ = io.WriteString(inW, "{\"type\":\"get_state\"}\n")
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("slow reader did not cancel the active turn after the write timeout and detach grace")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit")
	}
}
