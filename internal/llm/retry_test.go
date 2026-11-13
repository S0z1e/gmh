package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"
	"time"
)

func TestAPIErrorString(t *testing.T) {
	err := &APIError{StatusCode: 403, Status: "403 Forbidden", Body: "RBAC: access denied"}
	if got := err.Error(); got != "LLM API 403 Forbidden: RBAC: access denied" {
		t.Fatalf("APIError string = %q, want the pre-retry wording", got)
	}
}

func TestIsRetryable(t *testing.T) {
	urlErr := func(err error) error {
		return &url.Error{Op: "Post", URL: "http://example.invalid", Err: err}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 never", &APIError{StatusCode: 401, Status: "401 Unauthorized", Body: "bad key"}, false},
		{"403 retryable", &APIError{StatusCode: 403, Status: "403 Forbidden", Body: "RBAC"}, true},
		{"404 never", &APIError{StatusCode: 404, Status: "404 Not Found", Body: "no such model"}, false},
		{"400 never", &APIError{StatusCode: 400, Status: "400 Bad Request", Body: "bad json"}, false},
		{"422 never", &APIError{StatusCode: 422, Status: "422 Unprocessable Entity", Body: "bad input"}, false},
		{"429 retryable", &APIError{StatusCode: 429, Status: "429 Too Many Requests", Body: "slow down"}, true},
		{"500 retryable", &APIError{StatusCode: 500, Status: "500 Internal Server Error", Body: "boom"}, true},
		{"502 retryable", &APIError{StatusCode: 502, Status: "502 Bad Gateway", Body: "boom"}, true},
		{"503 retryable", &APIError{StatusCode: 503, Status: "503 Service Unavailable", Body: "boom"}, true},
		{"504 retryable", &APIError{StatusCode: 504, Status: "504 Gateway Timeout", Body: "boom"}, true},
		{"context canceled never", context.Canceled, false},
		{"context deadline never", context.DeadlineExceeded, false},
		{"idle timeout retryable", fmt.Errorf("%w: no stream data for 60s from model %q", ErrIdleTimeout, "m"), true},
		{"url error wrapping canceled never", urlErr(context.Canceled), false},
		{"url error retryable", urlErr(errors.New("connection reset")), true},
		{"EOF retryable", io.EOF, true},
		{"unexpected EOF retryable", io.ErrUnexpectedEOF, true},
		{"stream cut off retryable", errors.New("stream ended without [DONE]"), true},
		{"plain error never", errors.New("boom"), false},
		{"nil never", nil, false},
	}
	for _, tc := range cases {
		if got := IsRetryable(tc.err); got != tc.want {
			t.Errorf("IsRetryable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	if got := RetryAfter(&APIError{StatusCode: 429, RetryAfter: 5 * time.Second}); got != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", got)
	}
	if got := RetryAfter(errors.New("boom")); got != 0 {
		t.Fatalf("RetryAfter(plain error) = %v, want 0", got)
	}
	if got := parseRetryAfter("30"); got != 30*time.Second {
		t.Fatalf("parseRetryAfter(\"30\") = %v, want 30s", got)
	}
	if got := parseRetryAfter("not-a-number"); got != 0 {
		t.Fatalf("parseRetryAfter(\"not-a-number\") = %v, want 0", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Fatalf("parseRetryAfter(\"\") = %v, want 0", got)
	}
}
