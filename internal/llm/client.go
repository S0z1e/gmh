// Package llm is a tiny OpenAI-compatible client, stdlib only.
// Three API modes, selected with Client.API ("chat", "responses" or
// "anthropic"):
//   - chat:      POST /v1/chat/completions, messages[], choices[0].message
//   - responses: POST /v1/responses, instructions + input[], output[]
//   - anthropic: POST /v1/messages, system + messages[], content blocks
//
// All three support SSE streaming and accumulate partial tool-call
// arguments, which is the part a non-streaming client gets wrong.
// The agent loop only sees the shared Message / ToolCall types; the
// translation to either wire format lives here.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Message struct {
	Kind         string     `json:"kind,omitempty"`
	ToolError    bool       `json:"tool_error,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Refusal      string     `json:"refusal,omitempty"`
	Role         string     `json:"role"`
	Content      string     `json:"content,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`

	// Execution provenance is stored in sessions, never sent to providers.
	ToolInvocations []ToolInvocation `json:"tool_invocations,omitempty"`
	InvocationID    string           `json:"invocation_id,omitempty"`

	// Images are attached images (user messages only). The session
	// file stores their path and media type, never the bytes; the
	// bytes are re-read from disk on --resume.
	Images []Image `json:"images,omitempty"`
	// AnthropicContent preserves provider blocks and signatures for exact replay.
	AnthropicContent []AnthropicContentBlock `json:"anthropic_content,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func Tool(name, description, parameters string) ToolDef {
	var t ToolDef
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = description
	t.Function.Parameters = json.RawMessage(parameters)
	return t
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// ReasoningTokens is the thinking part of the completion tokens:
	// a subset of CompletionTokens, never an extra charge. Filled
	// from completion_tokens_details.reasoning_tokens (chat) or
	// output_tokens_details.reasoning_tokens (responses); Anthropic
	// does not report it separately, so it stays 0 there.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// CachedTokens is filled from whichever field the provider returns:
	// top-level cached_tokens, prompt_cache_hit_tokens, or
	// prompt_tokens_details.cached_tokens. See Usage.normalize.
	CachedTokens              int    `json:"cached_tokens"`
	CacheUsageReported        bool   `json:"cache_usage_reported"`
	CacheUsageSource          string `json:"cache_usage_source,omitempty"`
	CacheReportedPromptTokens int    `json:"cache_reported_prompt_tokens"`
	PromptCacheHitTokens      int    `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails       struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chatRequest struct {
	Model         string            `json:"model"`
	Messages      []chatWireMessage `json:"messages"`
	Tools         []ToolDef         `json:"tools,omitempty"`
	Stream        bool              `json:"stream"`
	StreamOptions *streamOptions    `json:"stream_options,omitempty"`
	// MaxTokens caps the reply (max_tokens). 0 omits the field.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// ReasoningEffort is how hard a reasoning model thinks
	// (low/medium/high). Empty omits the field.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// PromptCacheKey is the stable per-session routing hint for
	// prefix caching (OpenAI chat completions). Empty omits it.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// streamOptions requests token usage in the final SSE chunk. Without
// include_usage, a spec-compliant provider (e.g. Intern-Discovery)
// sends no usage object at all in a streaming Chat Completions
// response, and the turn reports 0 tokens. Providers that include
// usage by default (e.g. OpenConsole/deepseek) are unaffected.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk is the wire shape for one Chat Completions SSE delta.
type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
			// ReasoningContent is the model's thinking for this delta
			// (DeepSeek / OpenConsole). It is not the answer and must
			// never enter msg.Content; it goes to onThinking instead.
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// --- Responses API wire shapes ---

type responsesTool struct {
	// Keep optional tool arguments optional; Responses otherwise attempts
	// to normalize schemas into strict mode, changing their contract.
	Strict      bool            `json:"strict"`
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions,omitempty"`
	Input        []any           `json:"input"`
	Tools        []responsesTool `json:"tools,omitempty"`
	Stream       bool            `json:"stream"`
	Store        bool            `json:"store"` // false: stateless, we resend full input (JSONL is the store)
	// MaxOutputTokens caps the reply. 0 omits the field.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	// Reasoning carries the effort level for a reasoning model.
	// Nil omits the field.
	Reasoning *reasoningParams `json:"reasoning,omitempty"`
}

// reasoningParams is the Responses API's reasoning object: only
// the effort level, nothing else.
type reasoningParams struct {
	Effort string `json:"effort,omitempty"`
}

type responsesUsage struct {
	CacheUsageReported bool `json:"-"`
	InputTokens        int  `json:"input_tokens"`
	OutputTokens       int  `json:"output_tokens"`
	TotalTokens        int  `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// responsesEvent is the common envelope for Responses SSE events.
type responsesEvent struct {
	Type      string `json:"type"`
	Delta     string `json:"delta"`
	ItemID    string `json:"item_id"`
	Arguments string `json:"arguments"`
	Item      struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
	Response struct {
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *responsesUsage `json:"usage"`
	} `json:"response"`
}

type Client struct {
	traceMu       sync.Mutex
	previousTrace *RequestTrace
	BaseURL       string // may be with or without /v1
	APIKey        string
	Model         string
	API           string // "chat" (default), "responses" or "anthropic"
	// MaxTokens is the per-response output budget, sent by all three
	// modes (chat: max_tokens, responses: max_output_tokens,
	// anthropic: max_tokens). 0 = defaultAnthropicMaxTokens.
	MaxTokens int
	// SamplingParams (e.g. temperature, top_p) is merged into every
	// request body of the two OpenAI modes. Anthropic
	// ignores it: samplingParams apply only to OpenAI-compatible
	// APIs. Structural keys (model, messages, tools, stream, ...) are
	// never overridden by it.
	SamplingParams map[string]any
	// Effort is how hard a reasoning model thinks: low, medium or
	// high ("": the provider default). Chat sends it as
	// reasoning_effort, responses as reasoning.effort, anthropic as
	// a thinking budget derived from MaxTokens (see
	// effortThinkingBudget). A model that does not reason ignores
	// it.
	Thinking string // empty selects a format from the model ID; enabled/adaptive override it
	Effort   string
	// Timeout is the total per-request cap (http.Client.Timeout):
	// 0 = DefaultTotalTimeout. It is a backstop for a request that
	// never finishes at all; a slow-but-alive stream is governed
	// by IdleTimeout instead, so a larger total is safe.
	Timeout time.Duration
	// IdleTimeout is the silence cap while streaming: if no SSE
	// line arrives for this long, the stream is treated as stalled
	// and aborted with ErrIdleTimeout (retryable, like a cut-off).
	// Any scanned line counts as activity — text, thinking deltas,
	// even heartbeat/empty lines — because a line proves the
	// connection is alive. 0 = DefaultIdleTimeout.
	IdleTimeout time.Duration
	// PromptCacheKey is a stable per-session routing hint for prefix
	// caching: chat completions send it as prompt_cache_key, the
	// session id, the same value on every request of the session.
	// Empty omits the field. A gateway may strip or ignore it — it
	// is a hint, not a guarantee — so cache wins are verified from
	// usage.prompt_tokens_details.cached_tokens, never assumed.
	// SamplingParams can never override it (see mergeSamplingParams).
	PromptCacheKey string
	// Headers are extra request headers from the config file
	// (provider + model). Authorization, Content-Type, Accept and
	// x-api-key are never taken from here: auth is the client's job.
	Headers map[string]string
	HTTP    *http.Client
}

// Mode returns the normalised API mode.
func (c *Client) Mode() string {
	switch strings.ToLower(strings.TrimSpace(c.API)) {
	case "responses":
		return "responses"
	case "anthropic":
		return "anthropic"
	}
	return "chat"
}

// ProviderName returns a short display name for the base URL host,
// used in the "real price unverified" label. Unknown hosts are shown
// as-is.
func (c *Client) ProviderName() string {
	host := strings.TrimSpace(c.BaseURL)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	switch {
	case strings.Contains(host, "openconsole"):
		return "OpenConsole"
	case strings.Contains(host, "intern-ai"):
		return "Discovery"
	default:
		return host
	}
}

// endpoint normalises baseURL to .../v1/chat/completions,
// .../v1/responses or .../v1/messages.
func (c *Client) endpoint() string {
	base := strings.TrimSuffix(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	base = strings.TrimSuffix(base, "/chat/completions")
	base = strings.TrimSuffix(base, "/responses")
	base = strings.TrimSuffix(base, "/messages")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	switch c.Mode() {
	case "responses":
		return base + "/responses"
	case "anthropic":
		return base + "/messages"
	}
	return base + "/chat/completions"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: c.totalTimeout()}
}

// DefaultTotalTimeout is the per-request backstop when the model
// sets no timeoutSeconds: 600s, up from the old hardcoded 180s.
// The idle guard catches a truly stuck stream within 60s, so a
// larger total is safe — it only fires on a request that never
// finishes at all.
const DefaultTotalTimeout = 600 * time.Second

// DefaultIdleTimeout is the silence cap when the model sets no
// idleTimeoutSeconds: 60s with no SSE line means the stream
// stalled.
const DefaultIdleTimeout = 60 * time.Second

func (c *Client) totalTimeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTotalTimeout
	}
	return c.Timeout
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout <= 0 {
		return DefaultIdleTimeout
	}
	return c.IdleTimeout
}

// ErrIdleTimeout is returned by Stream when a streaming response
// produces no SSE line for the idle timeout: the model stopped
// sending data mid-reply. It is a cut-off, not a cancellation:
// IsRetryable reports true, so streamStep discards the partial
// reply and replays the call. It is distinct from
// context.DeadlineExceeded (the total-timeout backstop, and the
// user aborting), which stays non-retryable.
var ErrIdleTimeout = errors.New("llm stream idle timeout")

// idleLineScanner pumps bufio.Scanner lines over a channel so the
// consumer can tell a slow stream from a stuck one: next returns
// the next line, or ErrIdleTimeout when no line arrives within
// idle. Every scanned line counts as activity — text, thinking
// deltas, heartbeat/empty lines — because a line proves the
// connection is alive. Close releases the pump goroutine; the
// caller must call it (defer) and close the response body.
type idleLineScanner struct {
	lines chan string
	errs  chan error
	stop  chan struct{}
	once  sync.Once
	idle  time.Duration
	model string
}

func newIdleLineScanner(body io.Reader, idle time.Duration, model string) *idleLineScanner {
	s := &idleLineScanner{
		lines: make(chan string, 1),
		errs:  make(chan error, 1),
		stop:  make(chan struct{}),
		idle:  idle,
		model: model,
	}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64*1024), maxSSEEventBytes)
		sc.Split(splitSSELines())
		for sc.Scan() {
			line := sc.Text()
			select {
			case s.lines <- line:
			case <-s.stop:
				return
			}
		}
		s.errs <- sc.Err()
	}()
	return s
}

// close stops the pump goroutine. Safe to call more than once.
func (s *idleLineScanner) close() {
	s.once.Do(func() { close(s.stop) })
}

// next returns the next scanned line. ok=false ends the stream:
// err nil means clean EOF, non-nil the read or idle failure.
func (s *idleLineScanner) next() (line string, ok bool, err error) {
	if s.idle > 0 {
		timer := time.NewTimer(s.idle)
		defer timer.Stop()
		select {
		case l, open := <-s.lines:
			if !open {
				return "", false, s.terminalErr()
			}
			return l, true, nil
		case <-timer.C:
			return "", false, fmt.Errorf("%w: no stream data for %s from model %q", ErrIdleTimeout, s.idle, s.model)
		}
	}
	l, open := <-s.lines
	if !open {
		return "", false, s.terminalErr()
	}
	return l, true, nil
}

// terminalErr returns the pump's final scanner error, if any. The
// pump sends it before closing the lines channel, so a
// non-blocking receive here is exact.
func (s *idleLineScanner) terminalErr() error {
	select {
	case err := <-s.errs:
		return err
	default:
		return nil
	}
}

func (c *Client) doPost(ctx context.Context, body []byte) (*http.Response, context.CancelFunc, error) {
	return c.doPostHeaders(ctx, body, nil, true)
}

// doPostHeaders is doPost plus extra headers and an auth switch.
// withBearer sets the Authorization header; the Anthropic mode
// passes false for api.anthropic.com, which reads x-api-key instead
// (and must not receive a Bearer header alongside it).
// Config headers (Client.Headers) are sent too, except auth and
// framing headers, which this function owns; the per-call headers
// win over config headers.
// It also returns the request context's cancel func: the caller
// owns cancel and must run it before closing the body, after consuming the
// stream. The line pump can still be reading after a terminal SSE event;
// cancelling first releases that read so Body.Close cannot wait on it.
// On any error the cancel is already called and nil is returned.
func (c *Client) doPostHeaders(ctx context.Context, body []byte, headers map[string]string, withBearer bool) (*http.Response, context.CancelFunc, error) {
	c.traceRequest(ctx, body)
	// The request runs on a derived context so the first-byte idle
	// deadline can abort it: if the server sends no response
	// headers within the idle timeout, the stream is stalled before
	// it started.
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if withBearer {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.Headers {
		switch strings.ToLower(k) {
		case "authorization", "content-type", "accept", "x-api-key":
			continue
		}
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")
	// Do waits for the response headers; without a deadline of its
	// own, a server that never answers hangs until the total
	// timeout. Race it against the idle timeout instead: silence
	// before the first byte is a stall like any other.
	type doResult struct {
		resp *http.Response
		err  error
	}
	ch := make(chan doResult, 1)
	go func() {
		resp, err := c.httpClient().Do(req)
		if reqCtx.Err() != nil && resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		ch <- doResult{resp, err}
	}()
	timer := time.NewTimer(c.idleTimeout())
	defer timer.Stop()
	var resp *http.Response
	select {
	case r := <-ch:
		if r.err != nil {
			cancel()
			return nil, nil, r.err
		}
		resp = r.resp
	case <-timer.C:
		cancel()
		return nil, nil, fmt.Errorf("%w: no response headers for %s from model %q", ErrIdleTimeout, c.idleTimeout(), c.Model)
	}
	traceResponseHeaders(ctx, resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		cancel()
		return nil, nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(b)),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	return resp, cancel, nil
}

// Chat is a non-streaming convenience wrapper around Stream.
func (c *Client) Chat(ctx context.Context, messages []Message, tools []ToolDef, onText func(string)) (Message, Usage, error) {
	var msg Message
	var usage Usage
	err := c.Stream(ctx, messages, tools, func(text string) {
		if onText != nil {
			onText(text)
		}
	}, func(m Message, u Usage) {
		msg = m
		usage = u
	}, nil)
	return msg, usage, err
}

// Stream dispatches to the selected API mode. onThinking, when
// non-nil, receives the model's thinking deltas as they stream
// (reasoning_content in chat, reasoning summary/text deltas in
// Responses, thinking_delta in Anthropic). Thinking is display-only:
// it is never part of the returned message's Content, and signatures
// / encrypted_content are never passed through it.
func (c *Client) Stream(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage), onThinking func(string)) (err error) {
	ctx, finish := startRequestTrace(ctx)
	defer func() { finish(err) }()
	done := onDone
	onDone = func(m Message, u Usage) {
		u.normalize()
		if done != nil {
			done(m, u)
		}
	}
	if c.APIKey == "" {
		return fmt.Errorf("missing API key: set OPENAI_API_KEY")
	}
	switch c.Mode() {
	case "responses":
		return c.streamResponses(ctx, messages, tools, onText, onDone, onThinking)
	case "anthropic":
		return c.streamAnthropic(ctx, messages, tools, onText, onDone, onThinking)
	}
	return c.streamChat(ctx, messages, tools, onText, onDone, onThinking)
}

// mergeSamplingParams merges free-form sampling parameters
// (temperature, top_p, ...) into an already-marshalled request body.
// Structural keys are never overridden: a sampling param
// must not rewrite the model, the messages, the tools, or the
// streaming/max-token fields the client itself controls.
func mergeSamplingParams(body []byte, params map[string]any) ([]byte, error) {
	if len(params) == 0 {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	for k, v := range params {
		switch strings.ToLower(k) {
		case "model", "messages", "tools", "tool_choice", "stream",
			"stream_options", "max_tokens", "max_completion_tokens",
			"max_output_tokens", "input", "instructions", "store", "system",
			"prompt_cache_key":
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

// reasoningForEffort builds the Responses API's reasoning object
// for an effort level, or nil when no effort is set.
func reasoningForEffort(effort string) *reasoningParams {
	if effort == "" {
		return nil
	}
	return &reasoningParams{Effort: effort}
}

func (c *Client) streamChat(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage), onThinking func(string)) error {
	req := chatRequest{Model: c.Model, Messages: toChatWireMessages(messages), Tools: tools, Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}, PromptCacheKey: c.PromptCacheKey}
	if c.MaxTokens > 0 {
		req.MaxTokens = &c.MaxTokens
	}
	if c.Effort != "" {
		req.ReasoningEffort = c.Effort
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	body, err = mergeSamplingParams(body, c.SamplingParams)
	if err != nil {
		return err
	}
	resp, cancel, err := c.doPost(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	defer cancel()

	var msg Message
	msg.Role = "assistant"
	var usage Usage
	type acc struct {
		id   string
		name string
		args strings.Builder
	}
	accs := map[int]*acc{}
	order := []int{}

	lines := newSSEScanner(resp.Body, c.idleTimeout(), c.Model, "chat")
	defer lines.close()
	sawDone := false
	for {
		data, ok, scanErr := lines.next()
		if !ok {
			if scanErr != nil {
				return scanErr
			}
			break
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			sawDone = true
			break
		}
		if err := streamProviderError("chat", data); err != nil {
			return err
		}
		var chunk streamChunk
		if err := decodeStreamJSON("chat", data, &chunk); err != nil {
			return err
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
			if usage.TotalTokens == 0 {
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
			usage.normalize()
			traceUsage(ctx, usage)
		}
		for _, ch := range chunk.Choices {
			if ch.Index != 0 {
				return protocolError("chat", "unexpected choice index %d", ch.Index)
			}
			if msg.FinishReason != "" && (ch.Delta.Content != "" || len(ch.Delta.ToolCalls) > 0 || ch.Delta.Refusal != "" || ch.Delta.ReasoningContent != "" || ch.FinishReason != "" && ch.FinishReason != msg.FinishReason) {
				return protocolError("chat", "delta after choice finished")
			}
			msg.Refusal += ch.Delta.Refusal
			if ch.FinishReason != "" {
				msg.FinishReason = ch.FinishReason
			}
			if ch.Delta.Content != "" {
				msg.Content += ch.Delta.Content
				if onText != nil {
					onText(ch.Delta.Content)
				}
			}
			// Thinking is display-only: forward it, never add it
			// to the answer.
			if ch.Delta.ReasoningContent != "" && onThinking != nil {
				onThinking(ch.Delta.ReasoningContent)
			}
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Index == nil || *tc.Index < 0 || tc.Type != "" && tc.Type != "function" {
					return protocolError("chat", "invalid tool call index or type")
				}
				idx := *tc.Index
				a, ok := accs[idx]
				if !ok {
					if tc.ID == "" || tc.Function.Name == "" {
						return protocolError("chat", "arguments arrived before tool identity")
					}
					a = &acc{}
					accs[idx] = a
					order = append(order, idx)
				}
				if tc.ID != "" {
					if a.id != "" && a.id != tc.ID {
						return protocolError("chat", "tool id changed at index %d", idx)
					}
					a.id = tc.ID
				}
				if tc.Function.Name != "" {
					if a.name != "" && a.name != tc.Function.Name {
						return protocolError("chat", "tool name changed at index %d", idx)
					}
					a.name = tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	// A stream that ends without [DONE] was cut off mid-reply; a
	// half-accumulated tool call would otherwise run as if it were
	// complete. Fail loudly instead of returning partial text.
	if !sawDone {
		return protocolError("chat", "stream ended without [DONE]")
	}
	for _, idx := range order {
		a := accs[idx]
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:       a.id,
			Type:     "function",
			Function: FunctionCall{Name: a.name, Arguments: a.args.String()},
		})
	}
	return completeStream("chat", msg, usage, onDone)
}

// toResponsesInput converts the shared Message history into Responses
// instructions + input items. System becomes instructions; everything
// else becomes typed items (message / function_call / function_call_output).
func toResponsesInput(messages []Message) (string, []any) {
	var instructions []string
	var input []any
	for _, m := range messages {
		switch m.Role {
		case "system":
			instructions = append(instructions, m.Content)
		case "user":
			var parts []any
			if m.Content != "" {
				parts = append(parts, map[string]any{
					"type": "input_text", "text": m.Content,
				})
			}
			for _, img := range imageParts(m) {
				parts = append(parts, map[string]any{
					"type": "input_image", "image_url": img.DataURL(),
				})
			}
			if len(parts) == 0 {
				parts = append(parts, map[string]any{
					"type": "input_text", "text": "",
				})
			}
			input = append(input, map[string]any{
				"role": "user", "content": parts,
			})
		case "assistant":
			if m.Content != "" {
				input = append(input, map[string]any{
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "output_text", "text": m.Content,
					}},
				})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			})
		}
	}
	return strings.Join(instructions, "\n"), input
}

// maxOutputTokensPtr returns nil for 0 so the field is omitted.
func maxOutputTokensPtr(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

func (c *Client) streamResponses(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage), onThinking func(string)) error {
	instructions, input := toResponsesInput(messages)
	var rtools []responsesTool
	for _, t := range tools {
		rtools = append(rtools, responsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	body, err := json.Marshal(responsesRequest{
		Model: c.Model, Instructions: instructions, Input: input,
		Tools: rtools, Stream: true, Store: false,
		MaxOutputTokens: maxOutputTokensPtr(c.MaxTokens),
		Reasoning:       reasoningForEffort(c.Effort),
	})
	if err != nil {
		return err
	}
	body, err = mergeSamplingParams(body, c.SamplingParams)
	if err != nil {
		return err
	}
	resp, cancel, err := c.doPost(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	defer cancel()

	var msg Message
	msg.Role = "assistant"
	var usage Usage
	type acc struct {
		callID   string
		name     string
		args     strings.Builder
		argsDone bool
		done     bool
	}
	accs := map[string]*acc{} // keyed by item_id
	order := []string{}
	// reasoningSeen tracks, per reasoning item id, whether its
	// summary already arrived as deltas. A reasoning item whose
	// text never streamed is backfilled once from its done event
	// (the summaryText || contentText pattern); an item already
	// seen as deltas is never repeated.
	reasoningSeen := map[string]bool{}
	// reasoningPartsText joins one reasoning item's content parts
	// into the text to backfill. encrypted_content is opaque and is
	// never among the parts; only summary/text parts reach here.
	reasoningPartsText := func(parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}) string {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	lines := newSSEScanner(resp.Body, c.idleTimeout(), c.Model, "responses")
	defer lines.close()
	// OpenConsole's Responses stream ends with response.completed and
	// no [DONE] (verified live), so completion — not [DONE] — is the
	// terminal marker here.
	sawCompleted := false
	for {
		data, ok, scanErr := lines.next()
		if !ok {
			if scanErr != nil {
				return scanErr
			}
			break
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		if err := streamProviderError("responses", data); err != nil {
			return err
		}
		var ev responsesEvent
		if err := decodeStreamJSON("responses", data, &ev); err != nil {
			return err
		}
		if ev.Type == "" {
			return protocolError("responses", "missing event type")
		}
		if u := ev.Response.Usage; u != nil {
			usage = u.sharedUsage()
			traceUsage(ctx, usage)
		}
		switch ev.Type {
		case "response.refusal.delta":
			msg.Refusal += ev.Delta
		case "response.output_text.delta":
			if ev.Delta != "" {
				msg.Content += ev.Delta
				if onText != nil {
					onText(ev.Delta)
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			// Reasoning summary/text deltas are thinking, not the
			// answer: forward them, never add them to msg.Content.
			if ev.Delta != "" {
				reasoningSeen[ev.ItemID] = true
				if onThinking != nil {
					onThinking(ev.Delta)
				}
			}
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				id := ev.Item.ID
				if id == "" {
					id = ev.Item.CallID
				}
				if id == "" || ev.Item.CallID == "" || ev.Item.Name == "" {
					return protocolError("responses", "function call missing identity")
				}
				if _, ok := accs[id]; ok {
					return protocolError("responses", "duplicate added item %q", id)
				}
				accs[id] = &acc{callID: ev.Item.CallID, name: ev.Item.Name}
				accs[id].args.WriteString(ev.Item.Arguments)
				order = append(order, id)
			}
		case "response.function_call_arguments.delta":
			id := ev.ItemID
			a, ok := accs[id]
			if !ok || a.done || a.argsDone {
				return protocolError("responses", "arguments delta outside open item %q", id)
			}
			a.args.WriteString(ev.Delta)
		case "response.function_call_arguments.done":
			id := ev.ItemID
			a, ok := accs[id]
			if !ok || a.done || a.argsDone {
				return protocolError("responses", "arguments done outside open item %q", id)
			}
			if ev.Arguments != "" {
				if a.args.Len() > 0 && !sameJSON(a.args.String(), ev.Arguments) {
					return protocolError("responses", "arguments done conflicts with deltas")
				}
				// done carries the full arguments; prefer it over fragments.
				a.args.Reset()
				a.args.WriteString(ev.Arguments)
			}
			a.argsDone = true
		case "response.output_item.done":
			if ev.Item.Type == "reasoning" {
				// Backfill a reasoning item whose summary never
				// streamed as deltas; skip it when the deltas
				// already carried the text.
				id := ev.Item.ID
				if id == "" {
					id = ev.ItemID
				}
				if !reasoningSeen[id] {
					if text := reasoningPartsText(ev.Item.Content); text != "" {
						reasoningSeen[id] = true
						if onThinking != nil {
							onThinking(text)
						}
					}
				}
			}
			if ev.Item.Type == "function_call" {
				id := ev.Item.ID
				if id == "" {
					id = ev.Item.CallID
				}
				a, ok := accs[id]
				if !ok {
					if id == "" {
						return protocolError("responses", "done item missing identity")
					}
					a = &acc{}
					accs[id] = a
					order = append(order, id)
				}
				if a.done {
					return protocolError("responses", "duplicate done item %q", id)
				}
				if a.callID != "" && ev.Item.CallID != "" && a.callID != ev.Item.CallID || a.name != "" && ev.Item.Name != "" && a.name != ev.Item.Name {
					return protocolError("responses", "done item identity changed")
				}
				if ev.Item.Arguments != "" && a.args.Len() > 0 && !sameJSON(a.args.String(), ev.Item.Arguments) {
					return protocolError("responses", "done item arguments changed")
				}
				if ev.Item.CallID != "" {
					a.callID = ev.Item.CallID
				}
				if ev.Item.Name != "" {
					a.name = ev.Item.Name
				}
				if ev.Item.Arguments != "" && a.args.Len() == 0 {
					a.args.WriteString(ev.Item.Arguments)
				}
				a.done = true
			}
			if ev.Item.Type == "message" {
				for _, part := range ev.Item.Content {
					if part.Text != "" && !strings.Contains(msg.Content, part.Text) {
						// Text already arrived via deltas; only fill if missing.
						if msg.Content == "" {
							msg.Content += part.Text
							if onText != nil {
								onText(part.Text)
							}
						}
					}
				}
			}
		case "response.incomplete":
			sawCompleted = true
			msg.FinishReason = "incomplete"
		case "response.completed":
			msg.FinishReason = "stop"
			sawCompleted = true
			for _, out := range ev.Response.Output {
				switch out.Type {
				case "reasoning":
					// Last-chance backfill: a reasoning item the
					// stream never delivered as deltas or a done
					// event. Guarded by reasoningSeen, so text
					// already forwarded is never repeated.
					id := out.ID
					if id == "" {
						id = out.CallID
					}
					if !reasoningSeen[id] {
						if text := reasoningPartsText(out.Content); text != "" {
							reasoningSeen[id] = true
							if onThinking != nil {
								onThinking(text)
							}
						}
					}
				case "function_call":
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: out.CallID, Type: "function", Function: FunctionCall{Name: out.Name, Arguments: out.Arguments}})
				case "message":
					if msg.Content == "" {
						for _, part := range out.Content {
							msg.Content += part.Text
						}
					}
				}
			}
		}
		if sawCompleted {
			break
		}
	}
	// A stream that ends without response.completed was cut off
	// mid-reply; fail loudly instead of running a half-accumulated
	// tool call as if it were complete.
	if !sawCompleted {
		return protocolError("responses", "stream ended without response.completed")
	}
	// Merge any call the completed event did not already list, from
	// the accumulated fragments — a partial completed list must not
	// drop a call the stream actually carried.
	have := map[string]ToolCall{}
	for _, tc := range msg.ToolCalls {
		if _, exists := have[tc.ID]; exists {
			return protocolError("responses", "duplicate completed call id %q", tc.ID)
		}
		have[tc.ID] = tc
	}
	streamIDs := map[string]bool{}
	for _, id := range order {
		a := accs[id]
		if streamIDs[a.callID] {
			return protocolError("responses", "duplicate streamed call id %q", a.callID)
		}
		streamIDs[a.callID] = true
		if tc, exists := have[a.callID]; exists {
			if tc.Function.Name != a.name || a.args.Len() > 0 && !sameJSON(tc.Function.Arguments, a.args.String()) {
				return protocolError("responses", "completed call conflicts with streamed call %q", a.callID)
			}
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: a.callID, Type: "function", Function: FunctionCall{Name: a.name, Arguments: a.args.String()}})
	}
	return completeStream("responses", msg, usage, onDone)
}
