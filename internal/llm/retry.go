package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

// parseRetryAfter reads a Retry-After header as either delta-seconds or an
// RFC 7231 HTTP-date. Past dates and unparsable values return zero duration.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// IsRetryable reports whether a failed LLM call is worth retrying:
// a transient provider failure, not a bad request or a dead key.
// Context errors are never retryable: a cancelled turn must end,
// not loop. Whether a retryable error is actually retried is the
// caller's decision: agent.streamStep replays a cut-off stream
// only after emitting a step_discard event (so renderers drop the
// partial output first), and never replays a completed message.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// An idle stream is a cut-off, not a cancellation: the model
	// stopped sending data, and replaying the call may succeed (a
	// silent gateway stall is transient, like the 403 waves). The
	// total timeout (context.DeadlineExceeded above) stays
	// non-retryable.
	if errors.Is(err, ErrIdleTimeout) {
		return true
	}
	var protocol *ProtocolError
	if errors.As(err, &protocol) {
		return true
	}
	var stream *StreamError
	if errors.As(err, &stream) {
		return stream.retryable()
	}
	var api *APIError
	if errors.As(err, &api) {
		switch api.StatusCode {
		case 403, 408, 429, 500, 502, 503, 504, 529:
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

// IsContextLengthExceeded reports whether the provider rejected the request
// because the prompt exceeded the model's context window. Callers can treat
// this as a reactive trigger for emergency compaction before retrying.
func IsContextLengthExceeded(err error) bool {
	if err == nil {
		return false
	}
	var text string
	var api *APIError
	if errors.As(err, &api) {
		if api.StatusCode != 400 && api.StatusCode != 413 {
			return false
		}
		text = strings.ToLower(api.Body)
	} else {
		var stream *StreamError
		if !errors.As(err, &stream) {
			return false
		}
		text = strings.ToLower(stream.Code + " " + stream.Message)
	}
	for _, needle := range []string{
		"context_length_exceeded",
		"maximum context length",
		"prompt is too long",
		"input is too long",
		"too many tokens",
		"exceeds the maximum number of tokens",
		"request_too_large",
		"context window",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
