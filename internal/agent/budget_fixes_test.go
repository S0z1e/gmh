package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/turnbudget"
)

func TestBudgetRetryDistinguishesRejectionFromUnknownUsage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantCalls int
	}{
		{"rate limit", http.StatusTooManyRequests, 2},
		{"gateway error", http.StatusBadGateway, 1},
		{"connection lost", 0, 1},
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
			if err != nil || calls != tc.wantCalls {
				t.Fatalf("calls=%d, ledger=%+v, errors=%v/%v", calls, totals, runErr, err)
			}
			if tc.wantCalls == 2 {
				if runErr != nil || text != "recovered" || totals.RejectedAttempts != 1 || !totals.TokenCountsComplete() {
					t.Fatalf("rate-limit rejection spoiled the budget: %q, %+v, %v", text, totals, runErr)
				}
				if _, _, err := c.Run(t.Context(), history); err != nil || c.LastTurnBudget.SessionReportedTokens != 10 {
					t.Fatalf("next turn did not inherit complete session usage: %+v, %v", c.LastTurnBudget, err)
				}
			} else {
				var unavailable *turnbudget.Unavailable
				if !errors.As(runErr, &unavailable) || totals.TokenCountsComplete() {
					t.Fatalf("unknown usage was accepted: %+v, %v", totals, runErr)
				}
			}
		})
	}
}

func fakeBudgetClient(transport stoppedJobTransport) *llm.Client {
	return &llm.Client{BaseURL: "https://example.invalid", API: "chat", Model: "fake", APIKey: "fake", HTTP: &http.Client{Transport: transport}}
}

func budgetResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
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
