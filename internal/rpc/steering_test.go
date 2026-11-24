package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/turnbudget"
)

func TestInlineSteeringAcknowledgesQueuedTurnBeforeStart(t *testing.T) {
	s, _ := usageRegressionServer(t, false)
	d := startDriver(t, s.app, s.runner)
	d.waitFor("ready", time.Second)
	d.send(map[string]any{"id": "inline", "type": "steer_prompt", "message": "Run this as a new turn"})
	response := d.waitForResponse("inline", time.Second)
	data, _ := response["data"].(map[string]any)
	started := d.waitFor("turn_started", time.Second)
	if data["disposition"] != "queued" || data["turn_id"] != started["turn_id"] || response["sequence"].(float64) >= started["sequence"].(float64) {
		t.Fatalf("out-of-order inline acceptance: %v then %v", response, started)
	}
	d.waitFor("agent_settled", time.Second)
	d.quit(t)
}

func TestSteerPrompt(t *testing.T) {
	for _, first := range []fakeStep{
		{kind: "text", text: "Original answer"},
		{kind: "tool", tool: "write", args: map[string]any{"path": "obsolete.txt", "content": "obsolete"}},
	} {
		t.Run(first.kind, func(t *testing.T) {
			const timeout = 10 * time.Second
			const update = "Use the revised requirements."
			a, runner, root := setupTestApp(t, "full-access", []fakeStep{{kind: "text", text: "Unused."}})
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			requests := make(chan []llm.Message, 4)
			reply := fakeHandler(t, []fakeStep{first, {kind: "text", text: "Revised answer"}, {kind: "text", text: "Other answer"}})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Messages []llm.Message }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- req.Messages
				select {
				case <-release:
					reply(w, r)
				case <-r.Context().Done():
				}
			}))
			defer srv.Close()
			defer unblock()
			a.Cfg.Client.BaseURL = srv.URL
			d := startDriver(t, a, runner)
			t.Cleanup(func() { _ = d.inW.Close() })
			d.waitFor("ready", timeout)
			prompt := func(id, message, disposition string) string {
				t.Helper()
				d.send(map[string]any{"id": id, "type": "prompt", "message": message})
				resp := d.waitForResponse(id, timeout)
				data, _ := resp["data"].(map[string]any)
				if turnID, _ := data["turn_id"].(string); turnID != "" && data["disposition"] == disposition {
					return turnID
				}
				t.Fatalf("prompt response = %v, want %s and a turn ID", resp, disposition)
				return ""
			}
			active := prompt("start", "Original task", "started")
			d.waitFor("turn_started", timeout)
			select {
			case <-requests:
			case <-time.After(timeout):
				t.Fatal("first model request did not arrive")
			}
			other := prompt("other", "Run the unrelated task", "queued")
			selected := prompt("selected", update, "queued")
			d.send(map[string]any{"id": "stale", "type": "steer_prompt", "turn_id": selected, "active_turn_id": "earlier-turn"})
			if resp := d.waitFor("error", timeout); resp["id"] != "stale" {
				t.Fatalf("stale turn response = %v", resp)
			}
			for _, id := range []string{"steer", "retry"} {
				d.send(map[string]any{"id": id, "type": "steer_prompt", "turn_id": selected, "active_turn_id": active})
				resp := d.waitForResponse(id, timeout)
				data, _ := resp["data"].(map[string]any)
				if data["turn_id"] != selected || data["active_turn_id"] != active {
					t.Fatalf("steer response = %v", resp)
				}
			}
			unblock()
			steered := d.waitFor("prompt_steered", timeout)
			if steered["turn_id"] != active || steered["origin_turn_id"] != selected || steered["origin_request_id"] != "selected" {
				t.Fatalf("steered event = %v", steered)
			}
			select {
			case msgs := <-requests:
				count := 0
				for _, msg := range msgs {
					if msg.Role == "user" && msg.Content == update {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("next model request contains %d copies of steering, want 1", count)
				}
			case <-time.After(timeout):
				t.Fatal("model did not receive another request")
			}
			settled := d.waitFor("agent_settled", timeout)
			answer, _ := settled["text"].(string)
			if settled["turn_id"] != active || !strings.Contains(answer, "Revised answer") {
				t.Fatalf("active turn did not finish with the revised reply: %v", settled)
			}
			if started := d.waitFor("turn_started", timeout); started["turn_id"] != other {
				t.Fatalf("next standalone turn = %v, want %s", started, other)
			}
			d.waitFor("agent_settled", timeout)
			if _, err := os.Stat(filepath.Join(root, "obsolete.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("obsolete write should not run: %v", err)
			}
			d.send(map[string]any{"id": "state", "type": "get_state"})
			state, _ := d.waitForResponse("state", timeout)["data"].(map[string]any)
			if state["queued"] != float64(0) || state["turns"] != float64(2) {
				t.Fatalf("steering ran as a separate turn: %v", state)
			}
			messages, err := session.Load(a.Store.ID)
			if err != nil {
				t.Fatal(err)
			}
			saved := 0
			for _, msg := range messages {
				if msg.Role == "user" && msg.Content == update {
					if msg.Kind != "steering" || session.IsTurnStart(msg) {
						t.Fatalf("steering created a rollback boundary: %v", msg)
					}
					saved++
				}
			}
			if saved != 1 {
				t.Fatalf("history contains %d steering messages, want 1", saved)
			}
			d.quit(t)
		})
	}
}

func TestUnappliedSteeringKeepsQueueOrder(t *testing.T) {
	s := &Server{steeringTurn: "active", work: []workItem{
		{kind: workPrompt, req: Request{TurnID: "first", Message: "First"}},
		{kind: workPrompt, req: Request{TurnID: "selected", Message: "Selected"}},
	}}
	if err := s.scheduleSteering("selected", "active"); err != nil {
		t.Fatal(err)
	}
	s.receiveSteering(true, func(text string) bool {
		if text != "Selected" {
			t.Errorf("selected message = %q", text)
		}
		return false
	})
	s.closeSteering()
	if len(s.work) != 2 || s.work[0].req.TurnID != "first" || s.work[1].req.TurnID != "selected" {
		t.Fatalf("queue reordered or lost an unapplied prompt: %v", s.work)
	}
}

func TestSteeringDuringAutomaticApprovalSkipsWrite(t *testing.T) {
	a, runner, root := setupTestApp(t, "approve-for-me", []fakeStep{{kind: "text", text: "Unused."}})
	url, reviewing, release := blockSteeringProvider(t, 2, []fakeStep{
		{kind: "tool", tool: "write", args: map[string]any{"path": "obsolete.txt", "content": "obsolete"}},
		{kind: "text", text: "APPROVE"},
		{kind: "text", text: "Revised answer"},
	})
	a.Cfg.Client.BaseURL = url
	d := startDriver(t, a, runner)
	t.Cleanup(func() { _ = d.inW.Close() })
	d.waitFor("ready", steeringTestTimeout)
	active := sendSteeringTestPrompt(d, "original", "Write obsolete.txt")
	tool := d.waitFor("tool_start", steeringTestTimeout)
	waitForBlockedSteeringRequest(t, reviewing)
	selected := scheduleSteeringTestPrompt(d, active, "Do not write obsolete.txt.")
	release()

	assertSteeredPrompt(t, d, active, selected)
	ended := d.waitFor("tool_end", steeringTestTimeout)
	if tool["invocation_id"] == nil || ended["invocation_id"] != tool["invocation_id"] || ended["error"] == nil {
		t.Errorf("skipped tool should settle the original invocation: started=%v ended=%v", tool, ended)
	}
	if settled := d.waitFor("agent_settled", steeringTestTimeout); settled["turn_id"] != active || settled["reason"] != "end_turn" {
		t.Errorf("unexpected settlement: %v", settled)
	}
	if _, err := os.Stat(filepath.Join(root, "obsolete.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("obsolete write was dispatched: %v", err)
	}
	d.quit(t)
}

func TestSteeringAtExactTokenLimitStaysQueued(t *testing.T) {
	a, runner, _ := setupTestApp(t, "full-access", []fakeStep{{kind: "text", text: "Unused."}})
	limit := int64(15) // fakeHandler reports exactly 15 tokens per response.
	a.Cfg.TurnBudget = &turnbudget.Policy{MaxTokens: &limit}
	url, responding, release := blockSteeringProvider(t, 1, []fakeStep{
		{kind: "text", text: "Original answer"},
		{kind: "text", text: "Queued answer"},
	})
	a.Cfg.Client.BaseURL = url
	d := startDriver(t, a, runner)
	t.Cleanup(func() { _ = d.inW.Close() })
	d.waitFor("ready", steeringTestTimeout)
	active := sendSteeringTestPrompt(d, "original", "Original task")
	d.waitFor("turn_started", steeringTestTimeout)
	waitForBlockedSteeringRequest(t, responding)
	selected := scheduleSteeringTestPrompt(d, active, "Revised instructions")
	release()

	settled := d.waitFor("agent_settled", steeringTestTimeout)
	if settled["turn_id"] != active || settled["reason"] != "end_turn" {
		t.Fatalf("original turn should finish at the exact cap: %v", settled)
	}
	if started := d.waitFor("turn_started", steeringTestTimeout); started["turn_id"] != selected {
		t.Fatalf("unapplied prompt should start as a standalone turn: %v", started)
	}
	if settled := d.waitFor("agent_settled", steeringTestTimeout); settled["reason"] != "end_turn" {
		t.Errorf("standalone turn should finish with its own budget: %v", settled)
	}
	for _, ev := range d.pending {
		if ev["type"] == "prompt_steered" {
			t.Errorf("prompt reported as steered despite having no model request budget: %v", ev)
		}
	}
	d.quit(t)
}

func TestClearQueueThenAbortDropsClaimedSteering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		out: io.Discard, turnCancel: cancel, steeringTurn: "active",
		work: []workItem{{kind: workPrompt, req: Request{TurnID: "selected", Message: "Selected"}}},
	}
	if err := s.scheduleSteering("selected", "active"); err != nil {
		t.Fatal(err)
	}
	claimed, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s.receiveSteering(false, func(string) bool {
			close(claimed)
			<-ctx.Done()
			return false
		})
	}()
	<-claimed
	s.handleClearQueue(Request{})
	s.handleAbort(Request{})
	<-done
	if len(s.work) != 0 {
		t.Fatalf("cleared prompt is still queued: %v", s.work)
	}
}

const steeringTestTimeout = 10 * time.Second

func blockSteeringProvider(t *testing.T, request int32, steps []fakeStep) (string, <-chan struct{}, func()) {
	t.Helper()
	entered, released := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	var calls atomic.Int32
	reply := fakeHandler(t, steps)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == request {
			close(entered)
			select {
			case <-released:
			case <-r.Context().Done():
				return
			}
		}
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(release)
	return srv.URL, entered, release
}

func waitForBlockedSteeringRequest(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(steeringTestTimeout):
		t.Fatal("expected model request did not arrive")
	}
}

func sendSteeringTestPrompt(d *driver, id, message string) string {
	d.t.Helper()
	d.send(map[string]any{"id": id, "type": "prompt", "message": message})
	resp := d.waitForResponse(id, steeringTestTimeout)
	data, _ := resp["data"].(map[string]any)
	turnID, _ := data["turn_id"].(string)
	if turnID == "" {
		d.t.Fatalf("prompt response has no turn ID: %v", resp)
	}
	return turnID
}

func scheduleSteeringTestPrompt(d *driver, active, message string) string {
	d.t.Helper()
	selected := sendSteeringTestPrompt(d, "selected", message)
	d.send(map[string]any{"id": "steer", "type": "steer_prompt", "turn_id": selected, "active_turn_id": active})
	d.waitForResponse("steer", steeringTestTimeout)
	return selected
}

func assertSteeredPrompt(t *testing.T, d *driver, active, selected string) {
	t.Helper()
	if ev := d.waitFor("prompt_steered", steeringTestTimeout); ev["turn_id"] != active || ev["origin_turn_id"] != selected {
		t.Fatalf("steered event identifies the wrong turns: %v", ev)
	}
}
