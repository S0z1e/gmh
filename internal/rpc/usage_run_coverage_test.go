package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"minimalharness/internal/session"
	"net/http"
	"strings"
	"testing"
)

func TestUsageRunCoverageDistinguishesHistoricalUnknown(t *testing.T) {
	s, out := usageRegressionServer(t, true)
	valid := s.app.Cfg.Client.HTTP.Transport
	s.app.Cfg.Client.HTTP.Transport = usageRegressionTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n"))}, nil
	})
	s.runTurn(Request{Message: "missing usage"})
	if s.runner.RunUsageComplete() {
		t.Fatal("missing run usage presented as complete")
	}
	dec := json.NewDecoder(out)
	found := false
	for dec.More() {
		var event map[string]any
		if err := dec.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "agent_settled" {
			found = true
			if event["current_run_usage_complete"] != false {
				t.Fatalf("settled omitted run coverage: %+v", event)
			}
		}
	}
	if !found {
		t.Fatal("no settled event")
	}
	var printed strings.Builder
	s.runner.Print = func(format string, args ...any) { fmt.Fprintf(&printed, format, args...) }
	s.runner.Handle(context.Background(), "/usage")
	if !strings.Contains(printed.String(), "current-run usage incomplete") {
		t.Fatal("CLI labeled unknown run counts as zero: ", printed.String())
	}
	id := s.app.Store.ID
	s.app.Store.Close()
	reopened, err := session.Open(id, session.OpenOptions{Workspace: s.app.Root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.app.Store = reopened
	s.app.Cfg.Session = reopened
	s.app.Cfg.Client.HTTP.Transport = valid
	if err := s.runner.RefreshUsage(); err != nil {
		t.Fatal(err)
	}
	s.runTurn(Request{Message: "known new run"})
	if s.runner.SessionUsageComplete() || !s.runner.RunUsageComplete() {
		t.Fatal("historical gap tainted a measured new run")
	}
	for _, kind := range []string{"usage", "get_state"} {
		out.Reset()
		s.handleQuery(Request{Type: kind})
		var response struct{ Data map[string]any }
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data["current_run_usage_complete"] != true || response.Data["session_usage_complete"] != false {
			t.Fatalf("%s conflated coverage scopes: %+v", kind, response.Data)
		}
	}
}
