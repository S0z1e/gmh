package agent

import (
	"context"
	"net/http"
	"testing"

	"minimalharness/internal/llm"
)

func TestRequestTraceIncludesRetryAttempts(t *testing.T) {
	srv, calls := retryTestServer(t, 1, http.StatusForbidden, "")
	var events []Event
	cfg := retryTestConfig(t, srv, &events)
	cfg.TraceRequests = true
	_, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	var requests []Event
	for _, e := range events {
		if e.Kind == EventLLMRequest {
			requests = append(requests, e)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%d calls=%d", len(requests), *calls)
	}
	if requests[0].Attempt != 1 || requests[0].Request.StatusCode != 403 || requests[0].Request.Outcome != "error" || requests[1].Attempt != 2 || requests[1].Request.Relation != "identical" || requests[1].Request.RequestHash != requests[0].Request.RequestHash {
		t.Fatalf("requests=%+v", requests)
	}
}
