package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

func TestCanonicalToolKeyIgnoresArgOrder(t *testing.T) {
	a := CanonicalToolKey("read", `{"path":"a.txt","offset":1}`)
	b := CanonicalToolKey("read", `{"offset":1,"path":"a.txt"}`)
	if a != b {
		t.Fatalf("keys differ for reordered args: %q vs %q", a, b)
	}
	if CanonicalToolKey("read", `{"path":"b.txt"}`) == a {
		t.Fatal("different file must give a different key")
	}
	if CanonicalToolKey("edit", `{"path":"a.txt","offset":1}`) == a {
		t.Fatal("different tool must give a different key")
	}
}

func TestDoomLoopGuardThresholdAndSessionAllow(t *testing.T) {
	g := NewDoomLoopGuard(0)
	for i := 1; i < RepeatedToolCallThreshold; i++ {
		if detected, _ := g.Check("read", `{"path":"a.txt"}`); detected {
			t.Fatalf("call %d tripped the guard, want below threshold", i)
		}
	}
	detected, det := g.Check("read", `{"path":"a.txt"}`)
	if !detected || det == nil || det.RepeatCount != RepeatedToolCallThreshold {
		t.Fatalf("5th call: detected=%v det=%+v", detected, det)
	}
	// A different call breaks the streak.
	if detected, _ := g.Check("read", `{"path":"b.txt"}`); detected {
		t.Fatal("different args must break the streak")
	}
	// Session allow: the same call never trips again.
	for i := 0; i < RepeatedToolCallThreshold; i++ {
		g.Check("read", `{"path":"a.txt"}`)
	}
	g.AllowSession(CanonicalToolKey("read", `{"path":"a.txt"}`))
	for i := 0; i < RepeatedToolCallThreshold*2; i++ {
		if detected, _ := g.Check("read", `{"path":"a.txt"}`); detected {
			t.Fatalf("session-allowed call %d tripped the guard", i)
		}
	}
}

func TestDoomLoopGuardObserveClearsOnChangedOutput(t *testing.T) {
	g := NewDoomLoopGuard(0)
	key := CanonicalToolKey("read", `{"path":"a.txt"}`)
	for i := 0; i < 3; i++ {
		g.Check("read", `{"path":"a.txt"}`)
		g.Observe(key, "same output")
	}
	// The output changed: the call is making progress, streak clears.
	g.Observe(key, "different output")
	if detected, _ := g.Check("read", `{"path":"a.txt"}`); detected {
		t.Fatal("changed output must clear the streak")
	}
	// Identical output keeps the streak: 3 more identical calls trip.
	for i := 0; i < 3; i++ {
		g.Check("read", `{"path":"a.txt"}`)
		g.Observe(key, "same output")
	}
	if detected, _ := g.Check("read", `{"path":"a.txt"}`); !detected {
		t.Fatal("identical outputs must keep the streak to the threshold")
	}
}

func TestDoomLoopGuardAllowOnceRestartsStreak(t *testing.T) {
	g := NewDoomLoopGuard(0)
	key := CanonicalToolKey("read", `{"path":"a.txt"}`)
	for i := 0; i < RepeatedToolCallThreshold; i++ {
		g.Check("read", `{"path":"a.txt"}`)
	}
	g.AllowOnce(key)
	// One more repeat is count 2, not a trip; four more are needed.
	for i := 0; i < RepeatedToolCallThreshold-2; i++ {
		if detected, _ := g.Check("read", `{"path":"a.txt"}`); detected {
			t.Fatalf("call %d after allow-once tripped early", i)
		}
	}
	if detected, _ := g.Check("read", `{"path":"a.txt"}`); !detected {
		t.Fatal("threshold-1 further repeats after allow-once must trip again")
	}
}

// The loop ends the turn when the user denies a repeated call: the
// 5th identical call never executes, and the terminal reason is
// structured, not an error string to parse.
func TestRunDoomLoopDenyEndsTurn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var prompts int32
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 10,
		ConfirmContinuation: func(req ContinuationRequest) (ContinuationDecision, error) {
			atomic.AddInt32(&prompts, 1)
			if req.ToolName != "read" || req.RepeatCount != RepeatedToolCallThreshold {
				t.Errorf("continuation request = %+v", req)
			}
			return ContinuationDecision{Outcome: ContinuationDeny}, nil
		},
	}
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "read a.txt"}})
	if !errors.Is(err, ErrRepeatedToolCallDenied) {
		t.Fatalf("Run error = %v, want ErrRepeatedToolCallDenied", err)
	}
	if cfg.LastTerminalReason != TerminalRepeatedToolCallDenied {
		t.Fatalf("terminal reason = %q, want repeated_tool_call_denied", cfg.LastTerminalReason)
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Fatalf("continuation prompts = %d, want 1", got)
	}
	// 5 LLM calls: the 5th returns the repeated call, which is
	// denied before execution — so only 4 tool executions happened,
	// and the 5th call's history entry is the denial observation.
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("LLM calls = %d, want 5", got)
	}
}

func TestRunDoomLoopHeadlessDeniesWithoutPrompt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 10,
		// No ConfirmContinuation: headless must deny, never assume.
	}
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "read a.txt"}})
	if !errors.Is(err, ErrRepeatedToolCallDenied) {
		t.Fatalf("headless Run error = %v, want ErrRepeatedToolCallDenied", err)
	}
}
