package rpc

import (
	"bytes"
	"encoding/json"
	"io"
	"minimalharness/internal/agent"
	"minimalharness/internal/app"
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"minimalharness/internal/tools"
	"net/http"
	"strings"
	"testing"
)

type usageRegressionTransport func(*http.Request) (*http.Response, error)

func (f usageRegressionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func usageRegressionServer(t *testing.T, rates bool) (*Server, *bytes.Buffer) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := session.Open("native", session.OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	client := &llm.Client{API: "chat", Model: "fake", APIKey: "fake", BaseURL: "https://example.invalid", HTTP: &http.Client{Transport: usageRegressionTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))}, nil
	})}}
	a := &app.App{Root: root, Store: store, Cfg: &agent.Config{Client: client, Session: store, Exec: &tools.Executor{Root: root}, Mode: "auto", MaxLLMAttempts: 1}}
	if rates {
		a.Rates = llm.Rates{Configured: true, InputPerMTok: 1000, OutputPerMTok: 2000, Source: "test"}
		a.Cfg.UsageRates = a.Rates
	}
	r := &app.Runner{App: a, Print: func(string, ...any) {}}
	if err := r.RefreshUsage(); err != nil {
		t.Fatal(err)
	}
	out := new(bytes.Buffer)
	s := &Server{app: a, runner: r, out: out}
	a.Cfg.OnEvent = s.onEvent
	s.refreshState()
	return s, out
}
func TestUsageRegressionSettledHasCurrentRunCost(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	s.runTurn(Request{Message: "first"})
	s.runTurn(Request{Message: "second"})
	dec := json.NewDecoder(out)
	var last map[string]any
	for dec.More() {
		var event map[string]any
		if err := dec.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "agent_settled" {
			last = event
		}
	}
	t.Logf("settled=%+v", last)
	if last["current_run_cost_usd"] != 0.04 {
		t.Errorf("missing cumulative current-run cost: %v", last["current_run_cost_usd"])
	}
}
func TestUsageRegressionUnknownCurrentRunCostIsNotZero(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	s.runTurn(Request{Message: "first"})
	out.Reset()
	s.handleQuery(Request{Type: "usage", ID: "query"})
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	t.Logf("response=%+v", response)
	data := response["data"].(map[string]any)
	if v, exists := data["current_run_cost_usd"]; exists && v != nil {
		t.Errorf("unpriced current-run cost is reported as %v", v)
	}
}

func TestUsageRegressionFreeRunCostIsKnownZero(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	s.app.Rates = llm.Rates{Configured: true, Source: "explicit zero"}
	s.app.Cfg.UsageRates = s.app.Rates
	s.runTurn(Request{Message: "free"})
	out.Reset()
	s.handleQuery(Request{Type: "usage"})
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data := response["data"].(map[string]any)
	if v, exists := data["current_run_cost_usd"]; !exists || v != float64(0) {
		t.Fatalf("explicit free cost must be zero: %+v", data)
	}
}

func TestUsageRegressionMixedRunPricingRemainsUnknown(t *testing.T) {
	s, out := usageRegressionServer(t, false)
	s.runTurn(Request{Message: "unpriced"})
	s.app.Rates = llm.Rates{Configured: true, InputPerMTok: 1000, Source: "later rates"}
	s.app.Cfg.UsageRates = s.app.Rates
	s.runTurn(Request{Message: "priced"})
	out.Reset()
	s.handleQuery(Request{Type: "usage"})
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data := response["data"].(map[string]any)
	if _, exists := data["current_run_cost_usd"]; exists {
		t.Fatalf("partial pricing presented as complete: %+v", data)
	}
	if _, exists := data["cost_usd"]; exists {
		t.Fatalf("partial lifetime pricing presented as complete: %+v", data)
	}
}
