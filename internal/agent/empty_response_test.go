package agent

import (
	"context"
	"fmt"
	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmptyAndRefusalAreExplicitFailures(t *testing.T) {
	for _, tc := range []struct{ delta, want string }{{`{}`, "empty answer"}, {`{"refusal":"cannot help"}`, "refused"}} {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s,\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", tc.delta)
		}))
		cfg := &Config{Client: &llm.Client{BaseURL: srv.URL, APIKey: "fake", Model: "test"}, Exec: &tools.Executor{Root: t.TempDir()}, MaxSteps: 3}
		_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "task"}})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) || calls != 1 || cfg.LastTerminalReason != TerminalError {
			t.Fatalf("err=%v calls=%d reason=%s", err, calls, cfg.LastTerminalReason)
		}
	}
}
