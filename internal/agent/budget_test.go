package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"minimalharness/internal/turnbudget"
	"minimalharness/internal/verification"
)

func capInt(n int64) *int64     { return &n }
func capUSD(n float64) *float64 { return &n }
func capBool(b bool) *bool      { return &b }
func budgetConfig(t *testing.T, p *turnbudget.Policy) *Config {
	t.Helper()
	return &Config{Exec: &tools.Executor{Root: t.TempDir()}, Tools: ToolDefs(), Mode: "auto", TurnBudget: p}
}
func budgetCall(id, name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments = id, "function", name, args
	return tc
}
func budgetSSE(text string, calls []llm.ToolCall, tokens *int, done bool) string {
	delta := map[string]any{"content": text}
	if len(calls) > 0 {
		var wire []map[string]any
		for i, tc := range calls {
			wire = append(wire, map[string]any{"index": i, "id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments}})
		}
		delta["tool_calls"] = wire
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
	out := "data: " + string(b) + "\n\n"
	if tokens != nil {
		out += fmt.Sprintf("data: {\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":0,\"total_tokens\":%d}}\n\n", *tokens, *tokens)
	}
	if done {
		out += "data: [DONE]\n\n"
	}
	return out
}
func requireBudgetStop(t *testing.T, c *Config, events []Event, err error, reason TerminalReason) {
	t.Helper()
	if err == nil || c.LastTerminalReason != reason || c.LastTaskOutcome != OutcomeIncomplete {
		t.Fatalf("reason=%s outcome=%s err=%v", c.LastTerminalReason, c.LastTaskOutcome, err)
	}
	if countKind(events, EventDone) != 0 || countKind(events, EventError) != 1 {
		t.Fatalf("terminal events=%+v", events)
	}
	if !strings.Contains(events[len(events)-1].Text, "Task incomplete:") {
		t.Fatal("missing explicit incomplete report")
	}
}

func TestBudgetRejectsWholeBatchAndPersistsUnexecutedResults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxToolCalls: capInt(1)})
	store, err := session.Open("budget-batch", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	calls := []llm.ToolCall{budgetCall("a", "write", `{"path":"a","content":"a"}`), budgetCall("b", "write", `{"path":"b","content":"b"}`)}
	events, bodies, err := runVerification(t, c, budgetSSE("working", calls, nil, true))
	requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
	if len(bodies) != 1 || c.LastTurnBudget.ToolCalls != 0 {
		t.Fatal("rejected batch consumed slots or sent grace request")
	}
	for _, p := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(c.Exec.Root, p)); !os.IsNotExist(err) {
			t.Fatal("partial batch executed")
		}
	}
	history, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	results, outcomes := 0, 0
	for _, m := range history {
		if m.Role == "tool" {
			results++
			if !m.ToolError || m.InvocationID != "" || !strings.Contains(m.Content, "Not executed") {
				t.Fatalf("incorrect replay result=%+v", m)
			}
		}
		if m.Kind == "task_outcome" {
			outcomes++
			if !strings.Contains(m.Content, `"tool_calls":0`) || !strings.Contains(m.Content, `"outcome":"incomplete"`) {
				t.Fatal(m.Content)
			}
		}
	}
	if results != 2 || outcomes != 1 {
		t.Fatalf("results=%d outcomes=%d", results, outcomes)
	}
	// A new turn gets new counters, even after a failed turn.
	events, bodies, err = runVerification(t, c, budgetSSE("", calls[:1], nil, true), textSSE("done"))
	if err != nil || len(bodies) != 2 || c.LastTurnBudget.ToolCalls != 1 || countKind(events, EventDone) != 1 {
		t.Fatalf("next turn failed: %v", err)
	}
}

func TestBudgetTokenBoundariesAndMissingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens *int
		reason TerminalReason
	}{
		{"exact", newInt(10), ""}, {"overshoot", newInt(11), TerminalBudgetExceeded}, {"unknown", nil, TerminalBudgetUnavailable}, {"zero", newInt(0), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(10), StrictMetering: capBool(true)})
			events, bodies, err := runVerification(t, c, budgetSSE("done", nil, tc.tokens, true))
			if len(bodies) != 1 {
				t.Fatal("extra provider request")
			}
			if tc.reason != "" {
				requireBudgetStop(t, c, events, err, tc.reason)
			} else if err != nil || c.LastTerminalReason != TerminalEndTurn {
				t.Fatal(err)
			}
			if tc.tokens != nil && c.LastTurnBudget.ReportedTokens != int64(*tc.tokens) {
				t.Fatal("real usage not retained")
			}
		})
	}
}

func TestBudgetInvalidRequestsCountAndFinalAnswerAtToolCapIsAllowed(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxToolCalls: capInt(2)})
	calls := []llm.ToolCall{budgetCall("a", "write", `{"path":"no-content"}`), budgetCall("b", "unavailable", `{}`)}
	events, bodies, err := runVerification(t, c, budgetSSE("", calls, nil, true), textSSE("cannot do that"))
	if err != nil || len(bodies) != 2 || c.LastTurnBudget.ToolCalls != 2 || countKind(events, EventDone) != 1 {
		t.Fatalf("refusals or final answer counted incorrectly: %+v %v", c.LastTurnBudget, err)
	}
}

func TestBudgetReportedTotalDoesNotNeedPriceBreakdown(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(10)})
	response := budgetSSE("done", nil, newInt(10), true)
	response = strings.Replace(response, `"prompt_tokens":10,"completion_tokens":0,"total_tokens":10`, `"total_tokens":10,"cached_tokens":4`, 1)
	_, _, err := runVerification(t, c, response)
	if err != nil || c.LastTurnBudget.ReportedTokens != 10 || !c.LastTurnBudget.TokensComplete {
		t.Fatalf("known total erased by missing price breakdown: %+v %v", c.LastTurnBudget, err)
	}
}

func TestBudgetPriceCalculationUsesDecimalRates(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(.11)})
	c.UsageRates = llm.Rates{InputPerMTok: 1.1, OutputPerMTok: 1.1, CachedPerMTok: 1.1, Configured: true, Complete: true}
	_, cancel, err := c.startBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	u := llm.Usage{PromptTokens: 100000, TotalTokens: 100000}
	c.recordBudgetUsage(&u, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), true, true)
	if err := c.budgetError(); err != nil {
		t.Fatal("exact-cap charge rejected:", err)
	}
	if c.resources.tracker.State().RecordedCostUSD != .11 {
		t.Fatal("wrong cost")
	}
}

func TestBudgetCacheSplitOnlyRequiredByActivePrices(t *testing.T) {
	for _, peak := range []bool{false, true} {
		t.Run(fmt.Sprint(peak), func(t *testing.T) {
			c := budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(1)})
			c.UsageRates = llm.Rates{InputPerMTok: 1, OutputPerMTok: 1, CachedPerMTok: 1, PeakInputPerMTok: 3, PeakCachedPerMTok: 2, Configured: true, Complete: true}
			when := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			if peak {
				c.UsageRates.CachedPerMTok = 2
				c.UsageRates.PeakCachedPerMTok = 3
				when = time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
			}
			_, cancel, err := c.startBudget(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			u := llm.Usage{PromptTokens: 100, TotalTokens: 100}
			c.recordBudgetUsage(&u, when, true, true)
			if err := c.budgetError(); err != nil {
				t.Fatal("inactive prices forced cache split:", err)
			}
		})
	}
}

func TestBudgetApprovalExpiryPersistsKnownUnexecutedBatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxDurationSeconds: capInt(1)})
	c.Mode = "suggest"
	c.Ask = func(req ApprovalRequest) (bool, error) { <-req.Context.Done(); return false, req.Context.Err() }
	store, err := session.Open("expired-approval", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	calls := []llm.ToolCall{budgetCall("a", "write", `{"path":"a","content":"x"}`), budgetCall("b", "write", `{"path":"b","content":"x"}`)}
	events, _, err := runVerification(t, c, budgetSSE("", calls, nil, true))
	requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
	history, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	results := 0
	for _, m := range history {
		if m.Role == "tool" {
			results++
			if !strings.Contains(m.Content, "Not executed:") || !m.ToolError {
				t.Fatalf("invented unknown outcome for unexecuted call: %+v", m)
			}
		}
	}
	if results != 2 {
		t.Fatalf("results=%d", results)
	}
}
func newInt(n int) *int { return &n }

func TestBudgetMetersFailedAttemptsAndStopsRetryAtCap(t *testing.T) {
	for _, cap := range []int64{10, 20} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(cap)})
			events, bodies, err := runVerification(t, c, budgetSSE("partial", nil, newInt(10), false), budgetSSE("done", nil, newInt(10), true))
			if cap == 10 {
				requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
				if len(bodies) != 1 {
					t.Fatal("retry exceeded cap")
				}
			} else if err != nil || len(bodies) != 2 || c.LastTurnBudget.ReportedTokens != 20 {
				t.Fatalf("attempts unmetered: %+v %v", c.LastTurnBudget, err)
			}
		})
	}
}

func TestBudgetGraceUsageAndUnavailablePrices(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(15)})
	c.MaxSteps = 1
	calls := []llm.ToolCall{budgetCall("a", "read", `{"path":"missing"}`)}
	events, bodies, err := runVerification(t, c, budgetSSE("", calls, newInt(10), true), budgetSSE("grace", nil, newInt(10), true))
	requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
	if len(bodies) != 2 || c.LastTurnBudget.ReportedTokens != 20 {
		t.Fatal("grace usage omitted")
	}
	c = budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(1), StrictMetering: capBool(true)})
	events, bodies, err = runVerification(t, c, textSSE("done"))
	requireBudgetStop(t, c, events, err, TerminalBudgetUnavailable)
	if len(bodies) != 0 {
		t.Fatal("sent paid request without prices")
	}
}

func TestBudgetPriceUsesAllAttemptsAndFreeRatesAreValid(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(.3), StrictMetering: capBool(true)})
	c.UsageRates = llm.Rates{InputPerMTok: 100000, OutputPerMTok: 100000, CachedPerMTok: 100000, Configured: true, Complete: true}
	calls := []llm.ToolCall{budgetCall("a", "read", `{"path":"missing"}`)}
	events, bodies, err := runVerification(t, c, budgetSSE("", calls, newInt(1), true), budgetSSE("done", nil, newInt(2), true))
	if err != nil || len(bodies) != 2 || c.LastTurnBudget.RecordedCostUSD != .3 {
		t.Fatalf("cost=%+v err=%v", c.LastTurnBudget, err)
	}
	c.UsageRates = llm.Rates{Configured: true, Complete: true}
	_, _, err = runVerification(t, c, budgetSSE("free", nil, newInt(100), true))
	if err != nil || c.LastTurnBudget.RecordedCostUSD != 0 {
		t.Fatal(err)
	}
	c.UsageRates = llm.Rates{InputPerMTok: 2, CachedPerMTok: 1, Configured: true, Complete: true}
	events, _, err = runVerification(t, c, budgetSSE("unknown cache", nil, newInt(1), true))
	requireBudgetStop(t, c, events, err, TerminalBudgetUnavailable)
}

func TestBudgetCompactionIsMeteredBeforeCheckpoint(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(1)})
	c.ContextLimit = 100
	c.CompactHistory = func(context.Context, []llm.Message) (string, llm.Usage, error) {
		return "summary", llm.Usage{PromptTokens: 2, TotalTokens: 2}, nil
	}
	// Keep the latest task large enough that pruning alone cannot fit it.
	_, cancel, err := c.startBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	_, usage, err := c.compactMessages(context.Background(), []llm.Message{{Role: "user", Content: strings.Repeat("task ", 1000)}}, true)
	if err == nil || usage.TotalTokens != 2 || c.resources.tracker.State().ReportedTokens != 2 {
		t.Fatal("compaction escaped budget")
	}
}

func TestBudgetCustomCompactionUsingSummarizeDoesNotDoubleCharge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, budgetSSE("summary", nil, newInt(3), true))
	}))
	defer srv.Close()
	c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(3)})
	c.Client = &llm.Client{BaseURL: srv.URL, API: "chat", Model: "fake", APIKey: "fake"}
	c.CompactHistory = func(ctx context.Context, history []llm.Message) (string, llm.Usage, error) {
		return c.Summarize(ctx, history, nil)
	}
	ctx, cancel, err := c.startBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	_, _, err = c.compactMessages(ctx, []llm.Message{{Role: "user", Content: "work"}}, true)
	if err != nil || c.resources.tracker.State().ReportedTokens != 3 {
		t.Fatalf("double charged: %+v err=%v", c.resources.tracker.State(), err)
	}
}

func TestBudgetSummaryPreflightDoesNotInventProviderAttempt(t *testing.T) {
	for _, p := range []*turnbudget.Policy{{MaxTokens: capInt(100)}, {MaxCostUSD: capUSD(1)}} {
		c := budgetConfig(t, p)
		c.ContextLimit = 100
		c.Client = &llm.Client{API: "chat"}
		c.UsageRates = llm.Rates{Configured: true, Complete: true}
		c.CompactHistory = func(ctx context.Context, history []llm.Message) (string, llm.Usage, error) {
			return c.Summarize(ctx, history, nil)
		}
		ctx, cancel, err := c.startBudget(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = c.compactMessages(ctx, []llm.Message{{Role: "user", Content: "work"}}, true)
		cancel()
		state := c.resources.tracker.State()
		if err == nil || !strings.Contains(err.Error(), "no context input budget") || c.resources.attempts != 0 || !state.TokensComplete || !state.CostComplete || c.resources.failure != nil {
			t.Fatalf("preflight fabricated a request: err=%v attempts=%d state=%+v failure=%v", err, c.resources.attempts, state, c.resources.failure)
		}
	}
}

func TestBudgetDeadlineCancelsProviderAndPreservesUserAbort(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}))
			defer srv.Close()
			c := budgetConfig(t, &turnbudget.Policy{MaxDurationSeconds: capInt(1)})
			c.Client = &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: srv.URL}
			var events []Event
			c.OnEvent = func(e Event) { events = append(events, e) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if abort {
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			start := time.Now()
			_, _, err := c.Run(ctx, []llm.Message{{Role: "user", Content: "work"}})
			if time.Since(start) > 3*time.Second {
				t.Fatal("deadline did not interrupt provider")
			}
			if abort {
				if err == nil || c.LastTerminalReason != TerminalAbort || c.LastTaskOutcome != OutcomeAborted {
					t.Fatalf("abort misclassified: %s %v", c.LastTerminalReason, err)
				}
			} else {
				requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
			}
		})
	}
}

func TestBudgetDeadlineKeepsActualToolResultAndRejectsSuffix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxDurationSeconds: capInt(1)})
	store, err := session.Open("budget-deadline", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	calls := []llm.ToolCall{budgetCall("a", "bash", `{"command":"printf ran > marker; sleep 10"}`), budgetCall("b", "write", `{"path":"never","content":"x"}`)}
	events, _, err := runVerification(t, c, budgetSSE("", calls, nil, true))
	requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
	if _, err := os.Stat(filepath.Join(c.Exec.Root, "marker")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Exec.Root, "never")); !os.IsNotExist(err) {
		t.Fatal("suffix executed")
	}
	history, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range history {
		if m.Role == "tool" && m.ToolCallID == "a" && strings.Contains(m.Content, "Not executed") {
			t.Fatal("real side effect mislabeled")
		}
	}
}

func TestBudgetConcurrentJobsIncludesRetainedSessions(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxConcurrentJobs: capInt(1)})
	c.Exec.SwitchSession("earlier")
	job, err := c.Exec.BashBackground(context.Background(), tools.BashArgs{Command: "sleep 10"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { job.Cancel(); <-job.Done() }()
	c.Exec.SwitchSession("current")
	calls := []llm.ToolCall{budgetCall("a", "bash", `{"command":"printf unexpected","background":true}`)}
	events, bodies, err := runVerification(t, c, budgetSSE("", calls, nil, true))
	requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
	if len(bodies) != 1 || len(c.Exec.Jobs()) != 0 || c.Exec.RunningWorkspaceJobs() != 1 {
		t.Fatal("retained job did not consume quota")
	}
}

func TestBudgetWireCountsPreserveUnknownFieldsAcrossAPIs(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", api, complete), func(t *testing.T) {
				var response string
				switch api {
				case "chat":
					counts := `"prompt_tokens":2`
					if complete {
						counts += `,"completion_tokens":1`
					}
					response = "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}],\"usage\":{" + counts + "}}\n\ndata: [DONE]\n\n"
				case "responses":
					counts := `"input_tokens":2`
					if complete {
						counts += `,"output_tokens":1`
					}
					response = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{" + counts + "}}}\n\n"
				case "anthropic":
					response = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
					if complete {
						response += "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"
					}
					response += "data: {\"type\":\"message_stop\"}\n\n"
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, response)
				}))
				defer srv.Close()
				c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(10), StrictMetering: capBool(true)})
				c.Client = &llm.Client{BaseURL: srv.URL, Model: "fake", APIKey: "fake", API: api, HTTP: srv.Client()}
				var events []Event
				c.OnEvent = func(e Event) { events = append(events, e) }
				_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "work"}})
				if complete {
					if err != nil || c.LastTurnBudget.ReportedTokens != 3 || !c.LastTurnBudget.TokensComplete {
						t.Fatalf("usage=%+v err=%v", c.LastTurnBudget, err)
					}
				} else {
					requireBudgetStop(t, c, events, err, TerminalBudgetUnavailable)
					if c.LastTurnBudget.ReportedTokens != 2 || c.LastTurnBudget.TokensComplete {
						t.Fatalf("subtotal lost or called complete: %+v", c.LastTurnBudget)
					}
				}
			})
		}
	}
}

func TestBudgetResponsesNonterminalUsageRemainsPartial(t *testing.T) {
	response := "data: {\"type\":\"response.created\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":0,\"total_tokens\":2}}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, response)
	}))
	defer srv.Close()
	c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(10), StrictMetering: capBool(true)})
	c.Client = &llm.Client{BaseURL: srv.URL, API: "responses", Model: "fake", APIKey: "fake"}
	var events []Event
	c.OnEvent = func(e Event) { events = append(events, e) }
	_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "work"}})
	requireBudgetStop(t, c, events, err, TerminalBudgetUnavailable)
	if c.LastTurnBudget.ReportedTokens != 2 || c.LastTurnBudget.TokensComplete {
		t.Fatal("nonterminal counters treated as final")
	}
}

func TestBudgetPartialUsageRetainsEarlierCounters(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		for _, final := range []string{`{}`, `{"output_tokens":3}`, `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`} {
			t.Run(api+"/"+final, func(t *testing.T) {
				response := "data: {\"type\":\"response.created\",\"response\":{\"usage\":{\"input_tokens\":10}}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":" + final + "}}\n\n"
				if api == "chat" {
					final = strings.ReplaceAll(strings.ReplaceAll(final, "input_tokens", "prompt_tokens"), "output_tokens", "completion_tokens")
					response = "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}],\"usage\":{\"prompt_tokens\":10}}\n\ndata: {\"usage\":" + final + "}\n\ndata: [DONE]\n\n"
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, response)
				}))
				defer srv.Close()
				c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(100), StrictMetering: capBool(true)})
				c.Client = &llm.Client{BaseURL: srv.URL, API: api, Model: "fake", APIKey: "fake"}
				_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "work"}})
				want := int64(10)
				if strings.Contains(final, ":3") {
					want = 13
				}
				complete := strings.Contains(final, "total_tokens")
				if complete {
					want = 0 // A present zero supersedes earlier usage; an absent counter does not.
				}
				if c.LastTurnBudget.ReportedTokens != want || c.LastTurnBudget.TokensComplete != complete || (err == nil) != complete {
					t.Fatalf("state=%+v err=%v want tokens=%d complete=%v", c.LastTurnBudget, err, want, complete)
				}
			})
		}
	}
}

func TestBudgetCustomCompactionAncillaryFieldsDoNotProveZeroTokens(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(100), StrictMetering: capBool(true)})
	c.CompactHistory = func(context.Context, []llm.Message) (string, llm.Usage, error) {
		return "summary", llm.Usage{CacheUsageReported: true, ReasoningTokens: 2}, nil
	}
	ctx, cancel, err := c.startBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	_, _, err = c.compactMessages(ctx, []llm.Message{{Role: "user", Content: "work"}}, true)
	if err == nil || c.resources.tracker.State().TokensComplete {
		t.Fatalf("ancillary-only report became complete zero: %+v err=%v", c.resources.tracker.State(), err)
	}
}

func TestBudgetTerminalSettlementPrecedesEventCallback(t *testing.T) {
	for _, tokens := range []*int{newInt(1), nil} {
		t.Run(fmt.Sprint(tokens != nil), func(t *testing.T) {
			c := budgetConfig(t, &turnbudget.Policy{MaxDurationSeconds: capInt(1), MaxTokens: capInt(100), StrictMetering: capBool(true)})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, budgetSSE("done", nil, tokens, true))
			}))
			defer srv.Close()
			c.Client = &llm.Client{BaseURL: srv.URL, API: "chat", Model: "fake", APIKey: "fake"}
			var terminal []Event
			c.OnEvent = func(e Event) {
				if e.Kind == EventDone || e.Kind == EventError {
					terminal = append(terminal, e)
					deadline, _ := c.resources.tracker.Deadline()
					time.Sleep(time.Until(deadline) + 10*time.Millisecond)
				}
			}
			text, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "work"}})
			if len(terminal) != 1 {
				t.Fatalf("conflicting terminal events: %+v", terminal)
			}
			if tokens != nil {
				if err != nil || text != "done" || c.LastTerminalReason != TerminalEndTurn {
					t.Fatalf("published success changed: text=%q reason=%s err=%v", text, c.LastTerminalReason, err)
				}
			} else if err == nil || c.LastTerminalReason != TerminalBudgetUnavailable || terminal[0].Err.Error() != err.Error() {
				t.Fatalf("published budget failure changed: event=%+v err=%v", terminal[0], err)
			}
		})
	}
}

func TestBudgetCustomCompactionTotalOnlyIsNotFreePricing(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(1), StrictMetering: capBool(true)})
	c.UsageRates = llm.Rates{InputPerMTok: 1, OutputPerMTok: 1, CachedPerMTok: 1, Configured: true, Complete: true}
	c.CompactHistory = func(context.Context, []llm.Message) (string, llm.Usage, error) {
		return "summary", llm.Usage{TotalTokens: 5}, nil
	}
	ctx, cancel, err := c.startBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	_, _, err = c.compactMessages(ctx, []llm.Message{{Role: "user", Content: "work"}}, true)
	if err == nil || c.resources.tracker.State().CostComplete || c.resources.tracker.State().ReportedTokens != 5 {
		t.Fatalf("total-only callback recorded as free: %+v err=%v", c.resources.tracker.State(), err)
	}
}

func TestBudgetCustomCompactionNormalizesProgrammaticCacheUsage(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			c := budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(1), StrictMetering: capBool(true)})
			c.UsageRates = llm.Rates{InputPerMTok: 2, OutputPerMTok: 1, CachedPerMTok: 1, Configured: true, Complete: true}
			c.CompactHistory = func(context.Context, []llm.Message) (string, llm.Usage, error) {
				u := llm.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedTokens: 50}
				if partial {
					u.CacheUsageSource = "partial"
				}
				return "summary", u, nil
			}
			ctx, cancel, err := c.startBudget(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			_, _, err = c.compactMessages(ctx, []llm.Message{{Role: "user", Content: "work"}}, true)
			state := c.resources.tracker.State()
			if partial {
				if err == nil || state.CostComplete {
					t.Fatalf("partial cache split accepted: %+v err=%v", state, err)
				}
			} else if err != nil || !state.CostComplete || state.RecordedCostUSD != .00016 {
				t.Fatalf("programmatic cache split lost: %+v err=%v", state, err)
			}
		})
	}
}

func TestBudgetOutcomeStorageFailureEmitsOneTerminalError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(1)})
	store, err := session.Open("budget-storage", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	c.OnEvent = func(e Event) {
		if e.Kind == EventTextDelta {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// First fail while saving the budget outcome; then fail saving an ordinary
	// assistant response on the same Config, to verify per-turn publication reset.
	for _, tokens := range []int{2, 0} {
		events, _, err := runVerification(t, c, budgetSSE("done", nil, newInt(tokens), true))
		if !errors.Is(err, os.ErrClosed) || c.LastTerminalReason != TerminalError || c.LastTaskOutcome != OutcomeIncomplete || countKind(events, EventError) != 1 || countKind(events, EventDone) != 0 {
			t.Fatalf("storage failure settlement: reason=%s outcome=%s events=%+v err=%v", c.LastTerminalReason, c.LastTaskOutcome, events, err)
		}
		terminal := events[len(events)-1]
		if !errors.Is(terminal.Err, os.ErrClosed) {
			t.Fatalf("storage failure lost priority: %+v", terminal)
		}
	}
}

func TestBudgetDeadlineAndToolStorageFailurePreferOneStorageError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxDurationSeconds: capInt(1)})
	store, err := session.Open("budget-tool-storage", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	c.OnEvent = func(e Event) {
		if e.Kind == EventToolEnd {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			deadline, _ := c.resources.tracker.Deadline()
			time.Sleep(time.Until(deadline) + 10*time.Millisecond)
		}
	}
	calls := []llm.ToolCall{budgetCall("write", "write", `{"path":"changed","content":"x"}`)}
	events, _, err := runVerification(t, c, budgetSSE("working", calls, nil, true))
	if !errors.Is(err, os.ErrClosed) || c.LastTerminalReason != TerminalError || countKind(events, EventError) != 1 || countKind(events, EventDone) != 0 {
		t.Fatalf("conflicting storage/deadline settlement: reason=%s events=%+v err=%v", c.LastTerminalReason, events, err)
	}
	if !errors.Is(events[len(events)-1].Err, os.ErrClosed) {
		t.Fatal("terminal event lost storage priority")
	}
}

func TestBudgetContinuationExpiryPersistsUnexecutedSuffix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxDurationSeconds: capInt(1)})
	first := budgetCall("a", "write", `{"path":"a","content":"x"}`)
	c.DoomLoop().AllowOnce(CanonicalToolKey(first.Function.Name, first.Function.Arguments))
	c.DoomLoop().threshold = 2
	c.ConfirmContinuation = func(req ContinuationRequest) (ContinuationDecision, error) {
		<-req.Context.Done()
		return ContinuationDecision{}, req.Context.Err()
	}
	store, err := session.Open("expired-continuation", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	calls := []llm.ToolCall{first, budgetCall("b", "write", `{"path":"b","content":"x"}`)}
	events, _, err := runVerification(t, c, budgetSSE("", calls, nil, true))
	requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
	history, err := session.Load(store.ID)
	if err != nil {
		t.Fatal(err)
	}
	results := 0
	for _, m := range history {
		if m.Role == "tool" {
			results++
			if !strings.Contains(m.Content, "Not executed:") {
				t.Fatalf("missing known unexecuted result: %+v", m)
			}
		}
	}
	if results != 2 {
		t.Fatalf("results=%d", results)
	}
}

func TestBudgetCallerAbortBeforeDispatchAlwaysEmitsTerminal(t *testing.T) {
	for _, cancelAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(cancelAt), func(t *testing.T) {
			c := budgetConfig(t, &turnbudget.Policy{MaxDurationSeconds: capInt(0)})
			command := "printf ran > never"
			c.Verification = &verification.Policy{Required: []verification.Check{{Name: "check", Command: command}}}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			armed, checks := false, 0
			ctx := verificationCaptureContext{Context: parent, onErr: func() {
				if armed {
					checks++
					if checks == cancelAt {
						cancel()
					}
				}
			}}
			c.OnEvent = func(e Event) {
				if e.Kind == EventToolStart {
					armed = true
				}
			}
			events, _, err := runVerificationContext(t, c, ctx, verificationBash(command))
			if err == nil || c.LastTerminalReason != TerminalAbort || c.LastTaskOutcome != OutcomeAborted || countKind(events, EventError) != 1 {
				t.Fatalf("abort missing: reason=%s outcome=%s events=%+v err=%v", c.LastTerminalReason, c.LastTaskOutcome, events, err)
			}
			if _, err := os.Stat(filepath.Join(c.Exec.Root, "never")); !os.IsNotExist(err) {
				t.Fatal("cancelled call executed")
			}
		})
	}
}
