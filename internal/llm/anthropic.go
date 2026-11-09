// Anthropic Messages API support (third mode, "anthropic").
//
// POST <base>/v1/messages with stream:true. The wire format differs
// from the two OpenAI modes in three ways the loop never sees:
//   - auth: the Anthropic spec reads x-api-key, but Discovery's
//     Anthropic route reads Bearer and ignores x-api-key (verified
//     live 2026-11-09: Bearer 200, x-api-key 401). Worse, sending
//     both breaks Discovery: the gateway prefers x-api-key, and a
//     vault surrogate in that header is never exchanged for the
//     real key, so the request 401s even though the Bearer header
//     is valid (verified live 2026-11-09). The client therefore
//     picks one header by host: x-api-key only for api.anthropic.com,
//     Bearer only everywhere else. A gateway that reads the other
//     header will 401 with a clear error, not silently.
//   - history: system is a top-level string; assistant tool calls
//     are tool_use content blocks; tool results are tool_result
//     blocks inside a user message. Consecutive same-role messages
//     merge into one, because Anthropic requires strict
//     user/assistant alternation.
//   - reasoning: Agents-A1 emits thinking blocks before its answer.
//     They are skipped, like reasoning_content in chat mode.
//
// The event sequence was probed live against Discovery 2026-11-09:
// message_start, content_block_start (thinking, then text or
// tool_use), content_block_delta (text_delta / input_json_delta /
// thinking_delta), content_block_stop, message_delta (stop_reason
// and output_tokens), message_stop, and no [DONE].
package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// defaultAnthropicMaxTokens is the per-response output budget.
// Anthropic requires max_tokens; the OpenAI modes set none.
const defaultAnthropicMaxTokens = 8192

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
}

// anthropicEvent is the common envelope for Messages SSE events.
type anthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Usage anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// toAnthropicMessages converts the shared Message history into an
// Anthropic system string plus messages. System messages join into
// one string; assistant tool calls become tool_use blocks; tool
// results become tool_result blocks in a user message. Consecutive
// messages with the same role merge into one, because Anthropic
// requires strict user/assistant alternation. An empty tool result
// becomes "(no output)": a tool_result block needs content, and the
// placeholder says exactly what happened.
func toAnthropicMessages(messages []Message) (string, []anthropicMessage) {
	var system []string
	var msgs []anthropicMessage
	appendBlock := func(role string, b anthropicContentBlock) {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == role {
			msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, b)
			return
		}
		msgs = append(msgs, anthropicMessage{Role: role, Content: []anthropicContentBlock{b}})
	}
	for _, m := range messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				system = append(system, m.Content)
			}
		case "user":
			if m.Content != "" {
				appendBlock("user", anthropicContentBlock{Type: "text", Text: m.Content})
			}
		case "assistant":
			if m.Content != "" {
				appendBlock("assistant", anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if len(input) == 0 || !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				appendBlock("assistant", anthropicContentBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input,
				})
			}
		case "tool":
			content := m.Content
			if content == "" {
				content = "(no output)"
			}
			appendBlock("user", anthropicContentBlock{
				Type: "tool_result", ToolUseID: m.ToolCallID, Content: content,
			})
		}
	}
	return strings.Join(system, "\n"), msgs
}

// anthropicBearerAuth reports whether the Anthropic call should
// authenticate with a Bearer header. Real Anthropic
// (api.anthropic.com) reads x-api-key instead; every other host
// (including Discovery's compatibility route) reads Bearer.
func anthropicBearerAuth(baseURL string) bool {
	host := strings.TrimSpace(baseURL)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	host = strings.ToLower(strings.TrimSuffix(host, "/"))
	return host != "api.anthropic.com" && !strings.HasSuffix(host, ".anthropic.com")
}

func (c *Client) streamAnthropic(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage)) error {
	system, anthMsgs := toAnthropicMessages(messages)
	var atools []anthropicTool
	for _, t := range tools {
		atools = append(atools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}
	body, err := json.Marshal(anthropicRequest{
		Model: c.Model, MaxTokens: maxTokens, System: system,
		Messages: anthMsgs, Tools: atools, Stream: true,
	})
	if err != nil {
		return err
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	withBearer := anthropicBearerAuth(c.BaseURL)
	if !withBearer {
		headers["x-api-key"] = c.APIKey
	}
	resp, err := c.doPostHeaders(ctx, body, headers, withBearer)
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
		kind string
		args strings.Builder
	}
	accs := map[int]*acc{}
	order := []int{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sawStop := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			usage.PromptTokens = ev.Message.Usage.InputTokens
			usage.CachedTokens = ev.Message.Usage.CacheReadInputTokens
		case "content_block_start":
			a, ok := accs[ev.Index]
			if !ok {
				a = &acc{}
				accs[ev.Index] = a
				order = append(order, ev.Index)
			}
			a.kind = ev.ContentBlock.Type
			a.id = ev.ContentBlock.ID
			a.name = ev.ContentBlock.Name
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					msg.Content += ev.Delta.Text
					if onText != nil {
						onText(ev.Delta.Text)
					}
				}
			case "input_json_delta":
				a, ok := accs[ev.Index]
				if !ok {
					a = &acc{}
					accs[ev.Index] = a
					order = append(order, ev.Index)
				}
				a.args.WriteString(ev.Delta.PartialJSON)
				// thinking_delta and signature_delta are reasoning:
				// skipped, like reasoning_content in chat mode.
			}
		case "message_delta":
			usage.CompletionTokens = ev.Usage.OutputTokens
		case "message_stop":
			sawStop = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// A stream that ends without message_stop was cut off mid-reply;
	// fail loudly instead of running a half-accumulated tool call
	// as if it were complete.
	if !sawStop {
		return fmt.Errorf("stream ended without message_stop")
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	for _, idx := range order {
		a := accs[idx]
		if a.kind != "tool_use" || a.name == "" || a.id == "" {
			continue
		}
		args := a.args.String()
		if args == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID: a.id, Type: "function",
			Function: FunctionCall{Name: a.name, Arguments: args},
		})
	}
	if onDone != nil {
		onDone(msg, usage)
	}
	return nil
}
