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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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
	// CachedTokens is filled from whichever field the provider returns:
	// top-level cached_tokens, prompt_cache_hit_tokens, or
	// prompt_tokens_details.cached_tokens. See Usage.normalize.
	CachedTokens         int `json:"cached_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Tools         []ToolDef      `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	// MaxTokens caps the reply (max_tokens). 0 omits the field.
	MaxTokens *int `json:"max_tokens,omitempty"`
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
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// --- Responses API wire shapes ---

type responsesTool struct {
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
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
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
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage responsesUsage `json:"usage"`
	} `json:"response"`
}

type Client struct {
	BaseURL string // may be with or without /v1
	APIKey  string
	Model   string
	API     string // "chat" (default), "responses" or "anthropic"
	// MaxTokens is the per-response output budget, sent by all three
	// modes (chat: max_tokens, responses: max_output_tokens,
	// anthropic: max_tokens). 0 = defaultAnthropicMaxTokens.
	MaxTokens int
	// SamplingParams (e.g. temperature, top_p) is merged into every
	// request body of the two OpenAI modes, Pi-style. Anthropic
	// ignores it: Pi applies samplingParams only to OpenAI-compatible
	// APIs. Structural keys (model, messages, tools, stream, ...) are
	// never overridden by it.
	SamplingParams map[string]any
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
	return &http.Client{Timeout: 180 * time.Second}
}

func (c *Client) doPost(ctx context.Context, body []byte) (*http.Response, error) {
	return c.doPostHeaders(ctx, body, nil, true)
}

// doPostHeaders is doPost plus extra headers and an auth switch.
// withBearer sets the Authorization header; the Anthropic mode
// passes false for api.anthropic.com, which reads x-api-key instead
// (and must not receive a Bearer header alongside it).
// Config headers (Client.Headers) are sent too, except auth and
// framing headers, which this function owns; the per-call headers
// win over config headers.
func (c *Client) doPostHeaders(ctx context.Context, body []byte, headers map[string]string, withBearer bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
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
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("LLM API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
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
	})
	return msg, usage, err
}

// Stream dispatches to the selected API mode.
func (c *Client) Stream(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage)) error {
	if c.APIKey == "" {
		return fmt.Errorf("missing API key: set OPENAI_API_KEY")
	}
	switch c.Mode() {
	case "responses":
		return c.streamResponses(ctx, messages, tools, onText, onDone)
	case "anthropic":
		return c.streamAnthropic(ctx, messages, tools, onText, onDone)
	}
	return c.streamChat(ctx, messages, tools, onText, onDone)
}

// mergeSamplingParams merges free-form sampling parameters
// (temperature, top_p, ...) into an already-marshalled request body,
// Pi-style. Structural keys are never overridden: a sampling param
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
			"max_output_tokens", "input", "instructions", "store", "system":
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

func (c *Client) streamChat(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage)) error {
	req := chatRequest{Model: c.Model, Messages: messages, Tools: tools, Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}}
	if c.MaxTokens > 0 {
		req.MaxTokens = &c.MaxTokens
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	body, err = mergeSamplingParams(body, c.SamplingParams)
	if err != nil {
		return err
	}
	resp, err := c.doPost(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

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

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sawDone := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage.TotalTokens != 0 {
			usage = chunk.Usage
			usage.normalize()
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				msg.Content += ch.Delta.Content
				if onText != nil {
					onText(ch.Delta.Content)
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				a, ok := accs[tc.Index]
				if !ok {
					a = &acc{}
					accs[tc.Index] = a
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					a.id = tc.ID
				}
				if tc.Function.Name != "" {
					a.name = tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// A stream that ends without [DONE] was cut off mid-reply; a
	// half-accumulated tool call would otherwise run as if it were
	// complete. Fail loudly instead of returning partial text.
	if !sawDone {
		return fmt.Errorf("stream ended without [DONE]")
	}
	for _, idx := range order {
		a := accs[idx]
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:       a.id,
			Type:     "function",
			Function: FunctionCall{Name: a.name, Arguments: a.args.String()},
		})
	}
	if onDone != nil {
		onDone(msg, usage)
	}
	return nil
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
			input = append(input, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text", "text": m.Content,
				}},
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

func (c *Client) streamResponses(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage)) error {
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
	})
	if err != nil {
		return err
	}
	body, err = mergeSamplingParams(body, c.SamplingParams)
	if err != nil {
		return err
	}
	resp, err := c.doPost(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var msg Message
	msg.Role = "assistant"
	var usage Usage
	type acc struct {
		callID string
		name   string
		args   strings.Builder
	}
	accs := map[string]*acc{} // keyed by item_id
	order := []string{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	// OpenConsole's Responses stream ends with response.completed and
	// no [DONE] (verified live), so completion — not [DONE] — is the
	// terminal marker here.
	sawCompleted := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				msg.Content += ev.Delta
				if onText != nil {
					onText(ev.Delta)
				}
			}
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				id := ev.Item.ID
				if id == "" {
					id = ev.Item.CallID
				}
				if _, ok := accs[id]; !ok {
					accs[id] = &acc{callID: ev.Item.CallID, name: ev.Item.Name}
					order = append(order, id)
				}
			}
		case "response.function_call_arguments.delta":
			id := ev.ItemID
			a, ok := accs[id]
			if !ok {
				a = &acc{}
				accs[id] = a
				order = append(order, id)
			}
			a.args.WriteString(ev.Delta)
		case "response.function_call_arguments.done":
			id := ev.ItemID
			a, ok := accs[id]
			if !ok {
				a = &acc{}
				accs[id] = a
				order = append(order, id)
			}
			if ev.Arguments != "" {
				// done carries the full arguments; prefer it over fragments.
				a.args.Reset()
				a.args.WriteString(ev.Arguments)
			}
		case "response.output_item.done":
			if ev.Item.Type == "function_call" {
				id := ev.Item.ID
				if id == "" {
					id = ev.Item.CallID
				}
				a, ok := accs[id]
				if !ok {
					a = &acc{}
					accs[id] = a
					order = append(order, id)
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
		case "response.completed":
			sawCompleted = true
			for _, out := range ev.Response.Output {
				switch out.Type {
				case "function_call":
					// Completed carries the authoritative call list.
					found := false
					for _, tc := range msg.ToolCalls {
						if tc.ID == out.CallID {
							found = true
							_ = tc
						}
					}
					if !found {
						msg.ToolCalls = append(msg.ToolCalls, ToolCall{
							ID: out.CallID, Type: "function",
							Function: FunctionCall{Name: out.Name, Arguments: out.Arguments},
						})
					}
				case "message":
					if msg.Content == "" {
						for _, part := range out.Content {
							msg.Content += part.Text
						}
					}
				}
			}
			u := ev.Response.Usage
			if u.TotalTokens != 0 {
				usage = Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens, CachedTokens: u.InputTokensDetails.CachedTokens}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// A stream that ends without response.completed was cut off
	// mid-reply; fail loudly instead of running a half-accumulated
	// tool call as if it were complete.
	if !sawCompleted {
		return fmt.Errorf("stream ended without response.completed")
	}
	// Merge any call the completed event did not already list, from
	// the accumulated fragments — a partial completed list must not
	// drop a call the stream actually carried.
	have := map[string]bool{}
	for _, tc := range msg.ToolCalls {
		have[tc.ID] = true
	}
	for _, id := range order {
		a := accs[id]
		if a.name == "" || a.callID == "" || have[a.callID] {
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID: a.callID, Type: "function",
			Function: FunctionCall{Name: a.name, Arguments: a.args.String()},
		})
	}
	if onDone != nil {
		onDone(msg, usage)
	}
	return nil
}
