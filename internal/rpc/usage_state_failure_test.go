package rpc

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageRegressionGetStateAfterAccountingFailure(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	s.runTurn(Request{Message: "first"})
	previous := s.app.Cfg.Client.HTTP.Transport
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(r *http.Request) (*http.Response, error) {
		p := filepath.Join(filepath.Dir(s.app.Store.OutputDir()), "usage.jsonl")
		// Simulate an unavailable journal after the wire attempt has started.
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		return previous.RoundTrip(r)
	})
	s.runTurn(Request{Message: "second"})
	if s.runner.MeteringAvailable {
		t.Fatal("expected accounting failure")
	}
	out.Reset()
	s.handleQuery(Request{Type: "get_state"})
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	t.Logf("get_state=%+v current_run=%+v lastErr=%v", result, s.runner.RunUsage, s.runner.LastErr)
	data := result["data"].(map[string]any)
	if _, present := data["total_tokens"]; present && data["metering_available"] != false {
		t.Error("unreconciled stale lifetime usage is published without availability flag")
	}
}
