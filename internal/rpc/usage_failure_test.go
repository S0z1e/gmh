package rpc

import (
	"encoding/json"
	"io"
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
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
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
