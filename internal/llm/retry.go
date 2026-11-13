package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx response from the LLM provider. It carries
// the status code (for the retry decision) and the Retry-After
// header (for the backoff), while its Error string is identical to
// the plain error doPostHeaders used to return, so logs and existing
// error strings do not change.
type APIError struct {
	StatusCode int
	Status     string // e.g. "403 Forbidden"
	Body       string // response body, trimmed, capped by the caller
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("LLM API %s: %s", e.Status, e.Body)
}

// parseRetryAfter reads a Retry-After header as seconds. Only the
// seconds form is supported; an HTTP-date or an unparsable value is
// ignored (zero duration).
func parseRetryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// IsRetryable reports whether a failed LLM call is worth retrying:
// a transient provider failure, not a bad request or a dead key.
// Context errors are never retryable: a cancelled turn must end,
// not loop. Whether a retryable error is actually retried is the
// caller's decision: agent.streamStep never retries once any output
// was already exposed, even for a retryable error.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var api *APIError
	if errors.As(err, &api) {
		switch api.StatusCode {
		case 403, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if strings.HasPrefix(err.Error(), "stream ended without") {
		return true
	}
	return false
}

// RetryAfter returns the Retry-After delay carried by an APIError,
// or 0 when the error carries none.
func RetryAfter(err error) time.Duration {
	var api *APIError
	if errors.As(err, &api) {
		return api.RetryAfter
	}
	return 0
}
