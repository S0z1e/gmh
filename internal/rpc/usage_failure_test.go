package rpc

import (
	"encoding/json"
	"io"
	"minimalharness/internal/session"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageRegressionTotalOnlyPricingCoverage(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))}, nil
	})
	s.runTurn(Request{Message: "total only"})
	out.Reset()
	s.handleQuery(Request{Type: "usage"})
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data := response["data"].(map[string]any)
	t.Logf("total-only usage response: %+v", data)
	if _, exists := data["cost_usd"]; exists {
		t.Error("a missing input/output breakdown was reported as fully priced")
	}
	if _, exists := data["current_run_cost_usd"]; exists {
		t.Error("a missing input/output breakdown was reported as a known current-run cost")
	}
}

func TestUsageRegressionFailedAccountingCoverage(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	previous := s.app.Cfg.Client.HTTP.Transport
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
		path := filepath.Join(filepath.Dir(s.app.Store.OutputDir()), "usage.jsonl")
		// BeginUsageAttempt has created the empty journal before dispatch.
		// Replace it only now, to inject failure after the provider was called.
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
		return previous.RoundTrip(r)
	})
	s.runTurn(Request{Message: "accounting failure"})
	dec := json.NewDecoder(out)
	var settled map[string]any
	for dec.More() {
		var e map[string]any
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		if e["type"] == "agent_settled" {
			settled = e
		}
	}
	t.Logf("failed-accounting settled: %+v", settled)
	if settled["metering_available"] != false {
		t.Error("accounting failure was not marked unavailable")
	}
	if _, exists := settled["session_usage"]; exists {
		t.Error("unreconciled lifetime totals were published")
	}
	if settled["error"] == nil {
		t.Fatal("expected accounting error")
	}
	if _, exists := settled["current_run_cost_usd"]; exists {
		t.Error("failed accounting is reported as a known zero current-run cost")
	}
	if _, exists := settled["cost_usd"]; exists {
		t.Error("failed accounting is reported as a known zero turn cost")
	}
}

func TestUsageRegressionRecoveredGapWithholdsLifetimeCost(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	s.runTurn(Request{Message: "first"})
	path := filepath.Join(filepath.Dir(s.app.Store.OutputDir()), "usage.jsonl")
	s.app.Store.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":1,"id":"lost`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	store, err := session.Open("native", session.OpenOptions{Workspace: s.app.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.app.Store = store
	s.app.Cfg.Session = store
	if err := s.runner.RefreshUsage(); err != nil {
		t.Fatal(err)
	}
	s.refreshState()
	out.Reset()
	s.handleQuery(Request{Type: "usage"})
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	data := result["data"].(map[string]any)
	if _, ok := data["cost_usd"]; ok {
		t.Fatal("recovered gap advertised complete cost", data)
	}
	if data["session_usage_complete"] != false {
		t.Fatal("recovered gap advertised complete tokens", data)
	}
	s.runTurn(Request{Message: "new measured run"})
	if s.runner.CurrentRunCost() == nil {
		t.Fatal("historical gap erased known new-run cost")
	}
}
