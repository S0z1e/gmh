package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// RequestTrace describes the actual wire request without retaining its content
// or headers. Prefix comparisons are client-side JSON message comparisons, not
// a measurement of the provider's tokenized prefix or cache eligibility.
type RequestTrace struct {
	StartedAt      time.Time `json:"started_at"`
	RequestBytes   int       `json:"request_bytes"`
	ID             string    `json:"id"`
	PreviousID     string    `json:"previous_id,omitempty"`
	Model          string    `json:"model"`
	API            string    `json:"api"`
	EndpointHash   string    `json:"endpoint_hash"`
	CacheKeyHash   string    `json:"cache_key_hash,omitempty"`
	RequestHash    string    `json:"request_hash"`
	SystemHash     string    `json:"system_hash"`
	ToolsHash      string    `json:"tools_hash"`
	SettingsHash   string    `json:"settings_hash"`
	MessageHashes  []string  `json:"message_hashes"`
	SharedMessages int       `json:"shared_messages"`
	// FirstChangedMessage is zero-based. Nil means no old message changed.
	FirstChangedMessage *int   `json:"first_changed_message,omitempty"`
	Relation            string `json:"relation"` // baseline, identical, append, rewrite
	ScopeChanged        bool   `json:"scope_changed,omitempty"`
	SystemChanged       bool   `json:"system_changed,omitempty"`
	ToolsChanged        bool   `json:"tools_changed,omitempty"`
	SettingsChanged     bool   `json:"settings_changed,omitempty"`
	StatusCode          int    `json:"status_code,omitempty"`
	UpstreamRequestID   string `json:"upstream_request_id,omitempty"`
	HeadersMS           int64  `json:"headers_ms"`
	DurationMS          int64  `json:"duration_ms"`
	Outcome             string `json:"outcome"`
	Usage               *Usage `json:"usage,omitempty"`
	TokensComplete      bool   `json:"tokens_complete"`
	PriceCountsComplete bool   `json:"price_counts_complete"`
}

type observerKey struct{}

// RejectedBeforeInference identifies documented HTTP rejections with no usage.
// Transport failures and gateway errors can occur after inference began.
func (t RequestTrace) RejectedBeforeInference() bool {
	if t.Usage != nil || t.Outcome == "complete" {
		return false
	}
	switch t.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	}
	return false
}

// TransientFailure identifies gateway, timeout, or transport failures with
// unreported usage. A transient failure can occur after inference started.
func (t RequestTrace) TransientFailure() bool {
	if t.Usage != nil || t.Outcome == "complete" || t.RejectedBeforeInference() {
		return false
	}
	switch t.StatusCode {
	case 0, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

type requestStartKey struct{}
type requestAbortKey struct{}
type traceKey struct{}
type requestObservation struct {
	trace   RequestTrace
	started time.Time
	// Keep a wire total separately from totals derived from partial counters.
	wireTotal *int
}

// WithRequestObserver observes each Stream attempt, including failed attempts.
// The callback runs synchronously after the stream ends. No observer means no
// request hashing or prefix tracking overhead.
func WithRequestObserver(ctx context.Context, observe func(RequestTrace)) context.Context {
	return context.WithValue(ctx, observerKey{}, observe)
}

func startRequestTrace(ctx context.Context) (context.Context, func(error)) {
	observe, _ := ctx.Value(observerKey{}).(func(RequestTrace))
	start, _ := ctx.Value(requestStartKey{}).(func(RequestTrace) error)
	if observe == nil && start == nil {
		return ctx, func(error) {}
	}
	o := &requestObservation{started: time.Now()}
	ctx = context.WithValue(ctx, traceKey{}, o)
	return ctx, func(err error) {
		// Validation or marshal errors did not create a wire request.
		if o.trace.ID == "" {
			return
		}
		o.trace.DurationMS = time.Since(o.started).Milliseconds()
		o.trace.Outcome = "complete"
		if err != nil {
			o.trace.Outcome = "error"
		}
		if ctx.Err() != nil {
			o.trace.Outcome = "cancelled"
		}
		if observe != nil {
			observe(o.trace)
		}
	}
}

func hashBytes(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// WithRequestStart installs a synchronous persistence gate before HTTP dispatch.
// A failed gate prevents both dispatch and the completed-attempt observation.
func WithRequestStart(ctx context.Context, start func(RequestTrace) error) context.Context {
	return context.WithValue(ctx, requestStartKey{}, start)
}

// WithRequestAbort records that a prepared request never left the process.
// The callback receives the attempt id when one was assigned.
func WithRequestAbort(ctx context.Context, abort func(id string)) context.Context {
	return context.WithValue(ctx, requestAbortKey{}, abort)
}

func abortRequestTrace(ctx context.Context) {
	o, _ := ctx.Value(traceKey{}).(*requestObservation)
	abort, _ := ctx.Value(requestAbortKey{}).(func(string))
	if abort == nil || o == nil || o.trace.ID == "" {
		return
	}
	abort(o.trace.ID)
}
func (c *Client) traceRequest(ctx context.Context, body []byte) error {
	o, _ := ctx.Value(traceKey{}).(*requestObservation)
	if o == nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil
	}
	t := RequestTrace{ID: rand.Text(), Model: c.Model, API: c.Mode(), EndpointHash: hashBytes([]byte(c.endpoint())), RequestHash: hashBytes(body), Relation: "baseline"}
	t.StartedAt = o.started.UTC()
	t.RequestBytes = len(body)
	if key := fields["prompt_cache_key"]; len(key) > 0 {
		t.CacheKeyHash = hashBytes(key)
	}
	t.ToolsHash = hashBytes(fields["tools"])
	var messages []json.RawMessage
	messageField := "messages"
	if c.Mode() == "responses" {
		messageField = "input"
	}
	_ = json.Unmarshal(fields[messageField], &messages)
	var system []json.RawMessage
	for _, key := range []string{"system", "instructions"} {
		if value := fields[key]; len(value) > 0 {
			system = append(system, value)
		}
	}
	for _, m := range messages {
		t.MessageHashes = append(t.MessageHashes, hashBytes(m))
		var role struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(m, &role)
		if role.Role == "system" || role.Role == "developer" {
			system = append(system, m)
		}
	}
	encodedSystem, _ := json.Marshal(system)
	t.SystemHash = hashBytes(encodedSystem)
	for _, key := range []string{"model", "messages", "input", "system", "instructions", "tools", "prompt_cache_key"} {
		delete(fields, key)
	}
	settings, _ := json.Marshal(fields)
	t.SettingsHash = hashBytes(settings)
	if start, _ := ctx.Value(requestStartKey{}).(func(RequestTrace) error); start != nil {
		if err := start(t); err != nil {
			return err
		}
	}
	c.traceMu.Lock()
	if prev := c.previousTrace; prev != nil {
		t.PreviousID = prev.ID
		t.ScopeChanged = t.Model != prev.Model || t.API != prev.API || t.EndpointHash != prev.EndpointHash || t.CacheKeyHash != prev.CacheKeyHash
		t.SystemChanged = t.SystemHash != prev.SystemHash
		t.ToolsChanged = t.ToolsHash != prev.ToolsHash
		t.SettingsChanged = t.SettingsHash != prev.SettingsHash
		if !t.ScopeChanged {
			for t.SharedMessages < len(prev.MessageHashes) && t.SharedMessages < len(t.MessageHashes) && prev.MessageHashes[t.SharedMessages] == t.MessageHashes[t.SharedMessages] {
				t.SharedMessages++
			}
			t.Relation = "rewrite"
			if t.SharedMessages == len(prev.MessageHashes) {
				t.Relation = "append"
				if len(t.MessageHashes) == len(prev.MessageHashes) {
					t.Relation = "identical"
				}
			} else {
				index := t.SharedMessages
				t.FirstChangedMessage = &index
			}
		}
	}
	previous := t
	previous.MessageHashes = append([]string(nil), t.MessageHashes...)
	c.previousTrace = &previous
	c.traceMu.Unlock()
	o.trace = t
	return nil
}

func traceResponseHeaders(ctx context.Context, response *http.Response) {
	o, _ := ctx.Value(traceKey{}).(*requestObservation)
	if o == nil {
		return
	}
	o.trace.StatusCode = response.StatusCode
	o.trace.HeadersMS = time.Since(o.started).Milliseconds()
	// Only this provider-generated correlation header is retained. Never copy
	// arbitrary response headers, request headers, URL, or error bodies.
	id := response.Header.Get("X-Request-Id")
	if len(id) <= 256 {
		o.trace.UpstreamRequestID = id
	}
}

func traceUsage(ctx context.Context, usage Usage, tokensComplete, priceCountsComplete bool) {
	if o, _ := ctx.Value(traceKey{}).(*requestObservation); o != nil {
		// An attempt cannot regain valid metering after any invalid snapshot.
		usage.Invalid = !usage.Valid() || o.trace.Usage != nil && !o.trace.Usage.Valid()
		o.trace.Usage = &usage
		o.trace.TokensComplete = tokensComplete
		o.trace.PriceCountsComplete = priceCountsComplete
	}
}
