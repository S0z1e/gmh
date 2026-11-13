package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A stream that emits one text delta and then goes silent: the idle
// timeout must fire at ~idle, not at the stall's end or the total
// timeout — and the error must be the typed, retryable idle error,
// naming the idle period and the model.
func TestStreamIdleTimeoutAfterDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial-\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(3 * time.Second) // stall: the client must give up at the idle timeout
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", IdleTimeout: 200 * time.Millisecond, Timeout: 10 * time.Second}
	start := time.Now()
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {}, nil)
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if !strings.Contains(err.Error(), "200ms") || !strings.Contains(err.Error(), "test-model") {
		t.Fatalf("err = %v, want it to name the idle period and the model", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, want ~200ms (idle), not the 3s stall or the 10s total", elapsed)
	}
	if !IsRetryable(err) {
		t.Fatalf("IsRetryable(idle error) = false, want true: a stalled stream is a cut-off to replay")
	}
}

// A stream that sends nothing at all must idle out on every API
// mode: the idle guard lives in the shared line pump, not in one
// mode's loop. (The turn-2 Atria-Dawn death was exactly this: the
// first call of a fresh turn hung with no token at all.)
func TestStreamIdleTimeoutAllModes(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(1 * time.Second) // no byte at all
			}))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: api, IdleTimeout: 150 * time.Millisecond, Timeout: 10 * time.Second}
			start := time.Now()
			err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) {}, nil)
			if err == nil || !errors.Is(err, ErrIdleTimeout) {
				t.Fatalf("api %s: err = %v, want ErrIdleTimeout", api, err)
			}
			if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
				t.Fatalf("api %s: elapsed = %v, want ~150ms", api, elapsed)
			}
		})
	}
}

// A stream that keeps trickling tokens slower than any sane total
// but faster than the idle timeout must complete: slow but alive
// is not a stall. (A full 180s+ trickle is infeasible in a unit
// test; the invariant tested is that the idle timer resets on
// every line, so elapsed time never accumulates into a failure
// while lines keep arriving.)
func TestStreamTrickleSurvivesIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, word := range []string{"one", " two", " three", " four", " five", " six"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", word)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", IdleTimeout: 200 * time.Millisecond, Timeout: 10 * time.Second}
	var got Message
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, func(m Message, u Usage) { got = m }, nil)
	if err != nil {
		t.Fatalf("Stream error = %v, want nil: a trickling stream is alive", err)
	}
	if got.Content != "one two three four five six" {
		t.Fatalf("content = %q, want the six trickled words", got.Content)
	}
}
