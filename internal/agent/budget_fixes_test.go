package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/turnbudget"
)

func TestBudgetRetryDistinguishesRejectionFromUnknownUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"rate limit", http.StatusTooManyRequests},
		{"gateway error", http.StatusBadGateway},
		{"connection lost", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			c := budgetConfig(t, &turnbudget.Policy{MaxTokens: capInt(10), MaxSessionTokens: capInt(20)})
			store, err := session.Open("budget-retry", session.OpenOptions{Workspace: c.Exec.Root})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			c.Session = store
			calls := 0
			c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if tc.status == 0 {
						return nil, io.ErrUnexpectedEOF
					}
					return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.name))}, nil
				}
				return budgetResponse(budgetSSE("recovered", nil, newInt(5), true)), nil
			})
			history := []llm.Message{{Role: "user", Content: "work"}}
			text, _, runErr := c.Run(t.Context(), history)
			totals, err := store.UsageTotals()
			if err != nil || calls != 2 {
				t.Fatalf("calls=%d, ledger=%+v, errors=%v/%v", calls, totals, runErr, err)
			}
			rejected := tc.status == http.StatusTooManyRequests
			if runErr != nil || text != "recovered" || totals.TokenCountsComplete() != rejected || c.LastTurnBudget.TokensComplete != rejected {
				t.Fatalf("retryable failure spoiled the budget: %q, %+v, %v", text, totals, runErr)
			}
			if rejected && totals.RejectedAttempts != 1 {
				t.Fatalf("expected 1 rejected attempt, got %+v", totals)
			}
			if !rejected && (totals.TransientAttempts != 1 || totals.UnreportedAttempts != 1) {
				t.Fatalf("expected 1 transient attempt, got %+v", totals)
			}
			if _, _, err := c.Run(t.Context(), history); err != nil || c.LastTurnBudget.SessionReportedTokens != 10 || c.LastTurnBudget.BaselineIncomplete == rejected {
				t.Fatalf("next turn did not inherit the session metering evidence: %+v, %v", c.LastTurnBudget, err)
			}
			if rejected {
				return
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := session.Open("budget-retry", session.OpenOptions{Workspace: c.Exec.Root, Existing: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			c.Session = reopened
			c.TurnBudget.StrictMetering = capBool(true)
			_, _, err = c.Run(t.Context(), history)
			var unavailable *turnbudget.Unavailable
			if !errors.As(err, &unavailable) || unavailable.Resource != "session_tokens" || calls != 3 {
				t.Fatalf("strict resume accepted an unknown baseline: calls=%d, err=%v", calls, err)
			}
		})
	}
}

func TestStrictBudgetStopsOnTransientUnknownUsage(t *testing.T) {
	c := budgetConfig(t, &turnbudget.Policy{StrictMetering: capBool(true)})
	calls := 0
	c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("gateway error"))}, nil
	})
	_, _, err := c.Run(t.Context(), []llm.Message{{Role: "user", Content: "work"}})
	var unavailable *turnbudget.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Resource != "tokens" || calls != 1 {
		t.Fatalf("strict budget retried without usage evidence: calls=%d, err=%v", calls, err)
	}
}

func fakeBudgetClient(transport stoppedJobTransport) *llm.Client {
	return &llm.Client{BaseURL: "https://example.invalid", API: "chat", Model: "fake", APIKey: "fake", HTTP: &http.Client{Transport: transport}}
}

func budgetResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestManualCompactionAppliesTurnCostAndStrictMetering(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(0.05), StrictMetering: capBool(false)})
	c.UsageRates = llm.Rates{InputPerMTok: 1000, OutputPerMTok: 1000, CachedPerMTok: 1000, Configured: true, Complete: true}
	store, err := session.Open("compact-cost", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	calls := 0
	c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
		calls++
		return budgetResponse(budgetSSE("summary", nil, newInt(100), true)), nil
	})
	history := []llm.Message{{Role: "user", Content: "work"}}
	_, _, err = c.Compact(t.Context(), history)
	var exceeded *turnbudget.Exceeded
	if !errors.As(err, &exceeded) || exceeded.Resource != "cost_usd" || calls != 1 || c.LastTurnBudget == nil || !c.LastTurnBudget.CostCapEnforced {
		t.Fatalf("maxCostUSD-only compact was not enforced: calls=%d err=%v state=%+v", calls, err, c.LastTurnBudget)
	}

	c = budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(1), StrictMetering: capBool(true)})
	store, err = session.Open("compact-strict", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	c.UsageRates = llm.Rates{}
	c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("strict compact dispatched without rates")
		return nil, io.EOF
	})
	_, _, err = c.Compact(t.Context(), history)
	var unavailable *turnbudget.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Resource != "cost_usd" {
		t.Fatalf("strict compact did not stop before a provider attempt: %v", err)
	}
}

func TestManualCompactionHonorsExactSessionCostAfterReopen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxSessionCostUSD: capUSD(1)})
	c.UsageRates = llm.Rates{InputPerMTok: 1, CachedPerMTok: 1, Configured: true, Complete: true}
	open := func() *session.Store {
		t.Helper()
		store, err := session.Open("budget-compact", session.OpenOptions{Workspace: c.Exec.Root})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	store := open()
	defer store.Close()
	c.Session = store
	calls := 0
	c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
		calls++
		return budgetResponse(budgetSSE("done", nil, newInt(100000), true)), nil
	})
	history := []llm.Message{{Role: "user", Content: "work"}}
	for i := range 10 {
		if _, _, err := c.Run(t.Context(), history); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	c.Session = open()
	defer c.Session.Close()
	_, _, err := c.Compact(t.Context(), history)
	var exceeded *turnbudget.Exceeded
	if !errors.As(err, &exceeded) || exceeded.Resource != "session_cost_usd" || calls != 10 {
		t.Fatalf("manual compaction bypassed the persisted cap: calls=%d, err=%v", calls, err)
	}
}

func TestBudgetAnthropicCostBudgetWithCacheWriteRates(t *testing.T) {
	response := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100000,\"output_tokens\":0,\"cache_read_input_tokens\":100000,\"cache_creation_input_tokens\":100000}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":100000}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, response)
	}))
	defer srv.Close()
	c := budgetConfig(t, &turnbudget.Policy{MaxCostUSD: capUSD(2.0)})
	c.Client = &llm.Client{BaseURL: srv.URL, API: "anthropic", Model: "claude-sonnet", APIKey: "fake", HTTP: srv.Client()}
	c.UsageRates = llm.Rates{
		InputPerMTok:         3,
		OutputPerMTok:        15,
		CachedPerMTok:        0.3,
		CacheWritePerMTok:    3.75,
		CacheWriteConfigured: true,
		Configured:           true,
		Complete:             true,
		Source:               "config anthropic/claude-sonnet",
	}
	// Expected cost: 0.1*3 + 0.1*0.3 + 0.1*3.75 + 0.1*15 = 2.205 > 2.0
	var events []Event
	c.OnEvent = func(e Event) { events = append(events, e) }
	_, _, err := c.Run(context.Background(), []llm.Message{{Role: "user", Content: "work"}})
	requireBudgetStop(t, c, events, err, TerminalBudgetExceeded)
	if !c.LastTurnBudget.CostComplete || c.LastTurnBudget.RecordedCostUSD != 2.205 {
		t.Fatalf("unexpected anthropic cost state: %+v", c.LastTurnBudget)
	}
}

func TestApprovalTimeoutDeniesSingleCallWithoutKillingTurn(t *testing.T) {
	c := budgetConfig(t, nil)
	c.Mode = "suggest"
	c.Ask = func(req ApprovalRequest) (bool, error) {
		return false, fmt.Errorf("%w: timed out waiting for user", ErrApprovalTimedOut)
	}
	calls := []llm.ToolCall{budgetCall("a", "write", `{"path":"a.txt","content":"hello"}`)}
	events, bodies, err := runVerification(t, c, budgetSSE("", calls, newInt(5), true), budgetSSE("continued after timeout", nil, newInt(5), true))
	if err != nil || c.LastTerminalReason != TerminalEndTurn || len(bodies) != 2 || countKind(events, EventDone) != 1 {
		t.Fatalf("approval timeout killed turn: reason=%s bodies=%d err=%v", c.LastTerminalReason, len(bodies), err)
	}
}

func TestStrictMeteringNamesTheBlockingArtifact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	c := budgetConfig(t, &turnbudget.Policy{MaxSessionTokens: capInt(100), StrictMetering: capBool(true)})
	store, err := session.Open("strict-gap", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	id := "FIRSTID"
	if err := store.RecordUsage(session.UsageAttempt{ID: id, Request: llm.RequestTrace{ID: id, Usage: &llm.Usage{TotalTokens: 1}, TokensComplete: true}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session.Dir(), store.OutputID(), "usage.jsonl")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":1,"id":"torn`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	store, err = session.Open("strict-gap", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovery, err := store.UsageRecovery()
	if err != nil || len(recovery.Artifacts) != 1 {
		t.Fatalf("%+v %v", recovery, err)
	}
	c.Session = store
	c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
		t.Error("strict metering dispatched a provider request")
		return budgetResponse(budgetSSE("nope", nil, newInt(1), true)), nil
	})
	_, _, err = c.Run(t.Context(), []llm.Message{{Role: "user", Content: "work"}})
	if err == nil || !strings.Contains(err.Error(), recovery.Artifacts[0].Path) || !strings.Contains(err.Error(), "usage-repair") {
		t.Fatalf("strict error did not name the artifact and repair: %v", err)
	}
}

func TestUndispatchedRequestReconcilesAsNotDispatched(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	c := budgetConfig(t, nil)
	store, err := session.Open("not-sent", session.OpenOptions{Workspace: c.Exec.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c.Session = store
	c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("provider request left the process")
		return nil, io.EOF
	})
	c.Client.BaseURL = "://bad url"
	if _, _, err := c.Run(t.Context(), []llm.Message{{Role: "user", Content: "work"}}); err == nil {
		t.Fatal("expected the request to fail before dispatch")
	}
	report, err := store.ReconcileUsage()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, state := range report.States {
		found = found || state == "not_dispatched"
	}
	if !found || report.RecoveryGap {
		t.Fatalf("not-dispatched attempt was not reconciled: %+v", report)
	}
	totals, err := store.UsageTotals()
	if err != nil || totals.Attempts != 0 || totals.UnreportedAttempts != 0 {
		t.Fatalf("undispatched request was recorded as usage: %+v %v", totals, err)
	}
}

func TestMalformedProviderUsageDoesNotKillAnUncappedTurn(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprint(strict), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			var policy *turnbudget.Policy
			if strict {
				policy = &turnbudget.Policy{MaxTokens: capInt(100), StrictMetering: capBool(true)}
			} else {
				zero := int64(0)
				policy = &turnbudget.Policy{MaxTokens: &zero, MaxSessionTokens: &zero, MaxToolCalls: &zero, MaxDurationSeconds: &zero}
			}
			c := budgetConfig(t, policy)
			store, err := session.Open("bad-usage", session.OpenOptions{Workspace: c.Exec.Root})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			c.Session = store
			c.Client = fakeBudgetClient(func(*http.Request) (*http.Response, error) {
				usage := `{"prompt_tokens":-1,"completion_tokens":0,"total_tokens":-1}`
				if !strict {
					usage = `{"prompt_tokens":9223372036854775807,"completion_tokens":1,"total_tokens":9223372036854775807}`
				}
				body := "data: {\"choices\":[{\"delta\":{\"content\":\"kept\"}}],\"usage\":" + usage + "}\n\ndata: [DONE]\n\n"
				return budgetResponse(body), nil
			})
			text, _, err := c.Run(t.Context(), []llm.Message{{Role: "user", Content: "work"}})
			totals, totalsErr := store.UsageTotals()
			if totalsErr != nil || totals.InvalidAttempts != 1 {
				t.Fatalf("invalid usage was not quarantined: %+v %v", totals, totalsErr)
			}
			if strict {
				var unavailable *turnbudget.Unavailable
				if !errors.As(err, &unavailable) || c.LastTerminalReason != TerminalBudgetUnavailable {
					t.Fatalf("strict mode accepted malformed usage: %v reason=%s", err, c.LastTerminalReason)
				}
				return
			}
			if err != nil || text != "kept" || c.LastTerminalReason != TerminalEndTurn {
				t.Fatalf("non-strict uncapped turn died on malformed usage: text=%q reason=%s err=%v", text, c.LastTerminalReason, err)
			}
		})
	}
}
