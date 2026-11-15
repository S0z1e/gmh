package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		api, payload, code string
		retry              bool
	}{
		{"chat", `{"error":{"message":"too much context","type":"invalid_request_error","code":"context_length_exceeded"}}`, "context_length_exceeded", false},
		{"chat", `{"error":{"message":"busy","code":529}}`, "529", true},
		{"responses", `{"type":"error","message":"timeout","code":408}`, "408", true},
		{"responses", `{"type":"response.failed","response":{"error":{"message":"backend busy","code":"server_error"}}}`, "server_error", true},
		{"responses", `{"type":"error","error":{"message":"bad input","code":"context_length_exceeded"}}`, "context_length_exceeded", false},
		{"anthropic", `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, "overloaded_error", true},
		{"anthropic", `{"type":"error","error":{"type":"invalid_request_error","message":"bad thinking"}}`, "invalid_request_error", false},
	} {
		t.Run(tc.api+tc.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "data: %s\n\n", tc.payload) }))
			defer srv.Close()
			client := &Client{BaseURL: srv.URL, API: tc.api, Model: "test", APIKey: "fake"}
			done := false
			err := client.Stream(context.Background(), nil, nil, nil, func(Message, Usage) { done = true }, nil)
			var stream *StreamError
			if !errors.As(err, &stream) || !strings.Contains(err.Error(), tc.code) || IsRetryable(err) != tc.retry || done {
				t.Fatalf("error=%v retry=%v done=%v", err, IsRetryable(err), done)
			}
		})
	}
	for _, status := range []int{408, 529} {
		if !IsRetryable(&APIError{StatusCode: status}) {
			t.Fatalf("HTTP %d not retryable", status)
		}
	}
}
