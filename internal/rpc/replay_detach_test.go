package rpc

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRPCReplayTracksActualEvictionAcrossResponses(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	// Emit interleaved Response and Event frames so sequence numbers have gaps
	// between events even though nothing has been evicted yet.
	s.respond(Request{ID: "r1"}, map[string]any{"ok": true}, nil)
	s.writeEvent(Event{Type: "turn_started", Text: "first"})
	s.respond(Request{ID: "r2"}, map[string]any{"ok": true}, nil)
	s.writeEvent(Event{Type: "text_delta", Text: "hello"})

	out.Reset()
	s.handleQuery(Request{ID: "rep1", Type: "replay", AfterSequence: 1})
	var resp struct {
		Type string `json:"type"`
		Data struct {
			Truncated      bool    `json:"truncated"`
			LatestSequence uint64  `json:"latest_sequence"`
			Events         []Event `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode replay: %v (raw=%s)", err, out.String())
	}
	if resp.Data.Truncated {
		t.Fatalf("false positive truncated=true when no events were evicted: %+v", resp.Data)
	}
	if len(resp.Data.Events) != 2 {
		t.Fatalf("expected 2 replayed events, got %d", len(resp.Data.Events))
	}

	// Fill past maxReplayEvents to force real eviction, then verify truncated=true
	// for an old sequence and truncated=false for a retained sequence.
	for i := 0; i < maxReplayEvents+5; i++ {
		s.writeEvent(Event{Type: "text_delta", Text: "x"})
	}
	out.Reset()
	s.handleQuery(Request{ID: "rep2", Type: "replay", AfterSequence: 1})
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Data.Truncated {
		t.Fatalf("expected truncated=true after evicting sequence 1: %+v", resp.Data)
	}
	out.Reset()
	s.handleQuery(Request{ID: "rep3", Type: "replay", AfterSequence: resp.Data.LatestSequence})
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.Truncated || len(resp.Data.Events) != 0 {
		t.Fatalf("expected no truncation and 0 events at latest sequence: %+v", resp.Data)
	}
}

func TestRPCReplayPagesByBytesWithoutLosingEvents(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	large := strings.Repeat("x", 3<<20)
	for range 3 {
		s.writeEvent(Event{Type: "tool_end", Output: large})
	}
	readPage := func(after uint64) replayPage {
		t.Helper()
		out.Reset()
		s.handleQuery(Request{ID: "page", Type: "replay", AfterSequence: after})
		if out.Len() > maxOutputFrameBytes {
			t.Fatalf("page is %d bytes, exceeds frame limit %d", out.Len(), maxOutputFrameBytes)
		}
		var resp struct {
			Type string     `json:"type"`
			Data replayPage `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil || resp.Type != "response" {
			t.Fatalf("decode replay page: type=%q err=%v", resp.Type, err)
		}
		return resp.Data
	}
	first := readPage(0)
	if !first.HasMore || first.Truncated || first.NextAfterSequence != 2 || len(first.Events) != 2 {
		t.Fatalf("first page: more=%t truncated=%t next=%d events=%d", first.HasMore, first.Truncated, first.NextAfterSequence, len(first.Events))
	}
	last := readPage(first.NextAfterSequence)
	if last.HasMore || last.Truncated || last.NextAfterSequence != last.LatestSequence || len(last.Events) != 1 {
		t.Fatalf("last page: more=%t truncated=%t next=%d events=%d", last.HasMore, last.Truncated, last.NextAfterSequence, len(last.Events))
	}
	var ev Event
	if err := json.Unmarshal(last.Events[0], &ev); err != nil || ev.Sequence != 3 || ev.Output != large {
		t.Fatalf("unexpected last event: sequence=%d err=%v", ev.Sequence, err)
	}
	for range 3 {
		s.writeEvent(Event{Type: "tool_end", Output: large})
	}
	if page := readPage(0); !page.Truncated || page.OldestSequence != 2 {
		t.Fatalf("byte eviction: truncated=%t oldest=%d", page.Truncated, page.OldestSequence)
	}
}

func TestRPCOutputDisconnectAllowsActiveTurnToFinishInGraceWindow(t *testing.T) {
	t.Setenv("HARNESS_RPC_DETACH_GRACE_SECONDS", "5")
	s, _ := usageRegressionServer(t, false)
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			close(completed)
			body := "data: {\"choices\":[{\"delta\":{\"content\":\"persisted after disconnect\"}}]}\n\ndata: [DONE]\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()
	done := make(chan error, 1)
	go func() { done <- Run(s.app, s.runner, inR, outW) }()
	reader := bufio.NewReader(outR)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(inW, "{\"id\":\"p1\",\"type\":\"prompt\",\"message\":\"finish safely\"}\n"); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	// Disconnect stdout and trigger an output write; the active turn must NOT be
	// cancelled immediately while inside the detach grace window.
	outR.Close()
	_, _ = io.WriteString(inW, "{\"type\":\"get_state\"}\n")
	time.Sleep(150 * time.Millisecond)
	close(release)
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("active turn was cancelled before finishing in detach grace window")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit after detached turn finished")
	}
}

func TestOutputTimeoutPlusDetachGraceLetsTurnFinish(t *testing.T) {
	t.Setenv("HARNESS_RPC_DETACH_GRACE_SECONDS", "5")
	s, _ := usageRegressionServer(t, false)
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			close(completed)
			body := "data: {\"choices\":[{\"delta\":{\"content\":\"after both intervals\"}}]}\n\ndata: [DONE]\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()
	done := make(chan error, 1)
	go func() { done <- Run(s.app, s.runner, inR, outW) }()
	reader := bufio.NewReader(outR)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(inW, "{\"id\":\"p1\",\"type\":\"prompt\",\"message\":\"block then finish\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	outR.Close()
	_, _ = io.WriteString(inW, "{\"type\":\"get_state\"}\n")
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-completed:
		t.Fatal("turn was cancelled during the write timeout, before detach grace")
	default:
	}
	close(release)
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not finish inside the detach grace after the write timeout")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit")
	}
}
