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
//     They are displayed separately and preserved as blocks for replay.
//
// The event sequence was probed live against Discovery 2026-11-09:
// message_start, content_block_start (thinking, then text or
// tool_use), content_block_delta (text_delta / input_json_delta /
// thinking_delta), content_block_stop, message_delta (stop_reason
// and output_tokens), message_stop, and no [DONE].
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// defaultAnthropicMaxTokens is the per-response output budget.
// Anthropic requires max_tokens; the default is 16384.
const defaultAnthropicMaxTokens = 16384

// Known Claude 4.6+ models use adaptive thinking. Unknown compatible gateway
// model IDs retain the manual format; config can override aliases explicitly.
func anthropicThinkingMode(model string) string {
	parts := strings.Split(strings.TrimPrefix(strings.ToLower(model), "claude-"), "-")
	if strings.HasPrefix(strings.ToLower(model), "claude-") && len(parts) > 1 {
		if parts[0] == "mythos" || parts[0] == "fable" {
			return "adaptive"
		}
		version := parts[1:]
		if _, err := strconv.Atoi(parts[0]); err == nil {
			version = parts
		}
		major, err := strconv.Atoi(version[0])
		minor := 0
		if len(version) > 1 {
			minor, _ = strconv.Atoi(version[1])
		}
		if err == nil && (major >= 5 || (major == 4 && minor >= 6 && minor < 100)) {
			return "adaptive"
		}
	}
	return "enabled"
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicContentBlock is a replayable Messages API block. Signatures and
// redacted data are opaque; never render them as answer or thinking text.
type AnthropicContentBlock struct {
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Source carries an image block's base64 payload.
	Source *anthropicImageSource `json:"source,omitempty"`
}

func (b AnthropicContentBlock) MarshalJSON() ([]byte, error) {
	type plain AnthropicContentBlock
	if b.Type == "thinking" {
		return json.Marshal(struct {
			plain
			Thinking string `json:"thinking"`
		}{plain(b), b.Thinking})
	}
	return json.Marshal(plain(b))
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []AnthropicContentBlock `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
	// Thinking enables extended thinking with a token budget.
	// Nil omits the field (the provider default).
	Thinking     *anthropicThinking `json:"thinking,omitempty"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config,omitempty"`
}

// anthropicThinking is the Messages API's thinking object.
type anthropicThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// effortThinkingBudget maps a reasoning-effort level to an
// Anthropic thinking budget: a share of the output budget — low
// 1/16, medium 1/4, high 1/2 — because Anthropic has no
// low/medium/high knob, only a token budget, and the budget must
// stay below max_tokens (Anthropic's own rule). The floor is
// Anthropic's minimum of 1024 thinking tokens. With the default
// maxTokens of 16384 that is low 1024, medium 4096, high 8192.
// A maxTokens at or below 1024 cannot satisfy both the floor and
// the below-maxTokens rule; the budget then lands just under
// maxTokens and the provider returns a clear error.
func effortThinkingBudget(effort string, maxTokens int) int {
	var share int
	switch effort {
	case "low":
		share = maxTokens / 16
	case "medium":
		share = maxTokens / 4
	case "high":
		share = maxTokens / 2
	default:
		return 0
	}
	if share < 1024 {
		share = 1024
	}
	if share >= maxTokens {
		share = maxTokens - 1
	}
	return share
}

// anthropicEvent is the common envelope for Messages SSE events.
type anthropicEvent struct {
	Type  string `json:"type"`
	Index *int   `json:"index"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// Thinking is the text of a thinking_delta. Signature is
		// the opaque replay data of a signature_delta: never text,
		// never forwarded to onThinking.
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock AnthropicContentBlock `json:"content_block"`
	Message      struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	hasInput, hasOutput, hasCreation bool
	CacheCreationInputTokens         int  `json:"cache_creation_input_tokens"`
	CacheUsageReported               bool `json:"-"`
	InputTokens                      int  `json:"input_tokens"`
	OutputTokens                     int  `json:"output_tokens"`
	CacheReadInputTokens             int  `json:"cache_read_input_tokens"`
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
	appendBlock := func(role string, b AnthropicContentBlock) {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == role {
			msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, b)
			return
		}
		msgs = append(msgs, anthropicMessage{Role: role, Content: []AnthropicContentBlock{b}})
	}
	for _, m := range messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				system = append(system, m.Content)
			}
		case "user":
			// Images first, then the text: the same order as the
			// direct probe that verified Agents-A1 image input.
			for _, img := range imageParts(m) {
				appendBlock("user", AnthropicContentBlock{
					Type: "image",
					Source: &anthropicImageSource{
						Type: "base64", MediaType: img.MediaType, Data: img.Data,
					},
				})
			}
			if m.Content != "" {
				appendBlock("user", AnthropicContentBlock{Type: "text", Text: m.Content})
			}
			if text := unavailableImageText(m); text != "" {
				appendBlock("user", AnthropicContentBlock{Type: "text", Text: text})
			}
		case "assistant":
			if len(m.AnthropicContent) > 0 {
				for _, block := range anthropicReplayBlocks(m) {
					if block.Type == "text" && block.Text == "" {
						continue
					}
					appendBlock("assistant", block)
				}
				continue
			}
			if m.Content != "" {
				appendBlock("assistant", AnthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if len(input) == 0 || !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				appendBlock("assistant", AnthropicContentBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input,
				})
			}
		case "tool":
			content := m.Content
			if content == "" {
				content = "(no output)"
			}
			appendBlock("user", AnthropicContentBlock{
				Type: "tool_result", ToolUseID: m.ToolCallID, Content: content, IsError: m.ToolError,
			})
		}
	}
	return strings.Join(system, "\n"), msgs
}

// Context hooks may rewrite shared text/tool fields. Retain opaque thinking
// while reflecting those explicit edits in the otherwise exact block replay.
func anthropicReplayBlocks(m Message) []AnthropicContentBlock {
	var text strings.Builder
	for _, b := range m.AnthropicContent {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	changedText := text.String() != m.Content
	textWritten := false
	calls := map[string]ToolCall{}
	for _, tc := range m.ToolCalls {
		calls[tc.ID] = tc
	}
	seen := map[string]bool{}
	var blocks []AnthropicContentBlock
	for _, b := range m.AnthropicContent {
		switch b.Type {
		case "text":
			if changedText {
				if textWritten {
					continue
				}
				b.Text = m.Content
				textWritten = true
			}
			if b.Text == "" {
				continue
			}
		case "tool_use":
			tc, ok := calls[b.ID]
			if !ok {
				continue
			}
			b.Name = tc.Function.Name
			if string(b.Input) != tc.Function.Arguments {
				b.Input = json.RawMessage(tc.Function.Arguments)
			}
			if !json.Valid(b.Input) {
				b.Input = json.RawMessage("{}")
			}
			seen[b.ID] = true
		}
		blocks = append(blocks, b)
	}
	if changedText && !textWritten && m.Content != "" {
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		if !seen[tc.ID] {
			input := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, AnthropicContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
		}
	}
	return blocks
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

func (c *Client) streamAnthropic(ctx context.Context, messages []Message, tools []ToolDef, onText func(string), onDone func(Message, Usage), onThinking func(string)) error {
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
	anthReq := anthropicRequest{
		Model: c.Model, MaxTokens: maxTokens, System: system,
		Messages: anthMsgs, Tools: atools, Stream: true,
	}
	if c.Effort != "" {
		mode := c.Thinking
		if mode == "" {
			mode = anthropicThinkingMode(c.Model)
		}
		anthReq.Thinking = &anthropicThinking{Type: mode}
		if mode == "adaptive" {
			anthReq.OutputConfig = &struct {
				Effort string `json:"effort"`
			}{c.Effort}
		} else {
			if maxTokens <= 1024 {
				return fmt.Errorf("Anthropic enabled thinking requires maxTokens greater than 1024")
			}
			anthReq.Thinking.BudgetTokens = effortThinkingBudget(c.Effort, maxTokens)
		}
	}
	body, err := json.Marshal(anthReq)
	if err != nil {
		return err
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	withBearer := anthropicBearerAuth(c.BaseURL)
	if !withBearer {
		headers["x-api-key"] = c.APIKey
	}
	resp, cancel, err := c.doPostHeaders(ctx, body, headers, withBearer)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	defer cancel()

	var msg Message
	msg.Role = "assistant"
	var usage Usage
	var inputUsage anthropicUsage
	var finalOutputUsage bool
	recordUsage := func(delta *anthropicUsage) {
		if delta == nil {
			return
		}
		if delta.hasInput {
			inputUsage.InputTokens = delta.InputTokens
			inputUsage.hasInput = true
		}
		if delta.hasCreation {
			inputUsage.CacheCreationInputTokens = delta.CacheCreationInputTokens
			inputUsage.hasCreation = true
		}
		if delta.CacheUsageReported {
			inputUsage.CacheReadInputTokens = delta.CacheReadInputTokens
			inputUsage.CacheUsageReported = true
		}
		if delta.hasOutput {
			inputUsage.OutputTokens = delta.OutputTokens
			inputUsage.hasOutput = true
		}
		usage.PromptTokens = inputUsage.InputTokens + inputUsage.CacheReadInputTokens + inputUsage.CacheCreationInputTokens
		usage.CachedTokens = inputUsage.CacheReadInputTokens
		usage.CompletionTokens = inputUsage.OutputTokens
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.CacheUsageReported = inputUsage.CacheUsageReported
		if usage.CacheUsageReported {
			usage.CacheUsageSource = "cache_read_input_tokens"
		}
		usage.normalize()
		traceUsage(ctx, usage, finalOutputUsage && inputUsage.hasInput && inputUsage.hasOutput && inputUsage.hasCreation && inputUsage.CacheUsageReported, false)
	}
	type acc struct {
		id    string
		name  string
		kind  string
		args  strings.Builder
		block AnthropicContentBlock
		done  bool
	}
	accs := map[int]*acc{}
	order := []int{}

	lines := newSSEScanner(resp.Body, c.idleTimeout(), c.Model, "anthropic")
	defer lines.close()
	sawStop := false
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
		if err := streamProviderError("anthropic", data); err != nil {
			return err
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := decodeStreamJSON("anthropic", data, &envelope); err != nil {
			return err
		}
		if envelope.Type == "" {
			return protocolError("anthropic", "missing event type")
		}
		switch envelope.Type {
		case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop":
		default:
			continue
		}
		var ev anthropicEvent
		if err := decodeStreamJSON("anthropic", data, &ev); err != nil {
			return err
		}
		if ev.Type == "" {
			return protocolError("anthropic", "missing event type")
		}
		idx := 0
		if ev.Type == "content_block_start" || ev.Type == "content_block_delta" || ev.Type == "content_block_stop" {
			if ev.Index == nil || *ev.Index < 0 {
				return protocolError("anthropic", "missing or negative block index")
			}
			idx = *ev.Index
		}
		switch ev.Type {
		case "message_start":
			recordUsage(ev.Message.Usage)
		case "content_block_start":
			if _, ok := accs[idx]; ok {
				return protocolError("anthropic", "duplicate content block index %d", idx)
			}
			a := &acc{}
			accs[idx] = a
			order = append(order, idx)
			a.kind = ev.ContentBlock.Type
			a.id = ev.ContentBlock.ID
			a.name = ev.ContentBlock.Name
			a.block = ev.ContentBlock
			if a.kind == "text" && a.block.Text != "" {
				msg.Content += a.block.Text
				if onText != nil {
					onText(a.block.Text)
				}
			}
			if a.kind == "thinking" && a.block.Thinking != "" && onThinking != nil {
				onThinking(a.block.Thinking)
			}
		case "content_block_delta":
			current, ok := accs[idx]
			if !ok || current.done {
				return protocolError("anthropic", "delta outside open content block %d", idx)
			}
			switch ev.Delta.Type {
			case "text_delta":
				if current.kind != "text" {
					return protocolError("anthropic", "text delta in non-text block")
				}
				if a := accs[idx]; a != nil {
					a.block.Text += ev.Delta.Text
				}
				if ev.Delta.Text != "" {
					msg.Content += ev.Delta.Text
					if onText != nil {
						onText(ev.Delta.Text)
					}
				}
			case "thinking_delta":
				if current.kind != "thinking" {
					return protocolError("anthropic", "thinking delta in non-thinking block")
				}
				if a := accs[idx]; a != nil {
					a.block.Thinking += ev.Delta.Thinking
				}
				// Thinking is display-only: forward it, never add
				// it to the answer.
				if ev.Delta.Thinking != "" && onThinking != nil {
					onThinking(ev.Delta.Thinking)
				}
			case "signature_delta":
				if current.kind != "thinking" {
					return protocolError("anthropic", "signature delta in non-thinking block")
				}
				// Opaque replay data, not text: accumulate it and
				// never forward it to onThinking.
				if a := accs[idx]; a != nil {
					a.block.Signature += ev.Delta.Signature
				}
			case "input_json_delta":
				if current.kind != "tool_use" && current.kind != "server_tool_use" {
					return protocolError("anthropic", "input JSON delta in non-tool block")
				}
				current.args.WriteString(ev.Delta.PartialJSON)
				// redacted_thinking has no text: there is nothing
				// to forward, so it is ignored entirely.
			}
		case "content_block_stop":
			a, ok := accs[idx]
			if !ok || a.done {
				return protocolError("anthropic", "stop outside open content block %d", idx)
			}
			a.done = true
		case "message_delta":
			msg.FinishReason = ev.Delta.StopReason
			if ev.Usage != nil && ev.Usage.hasOutput {
				finalOutputUsage = true
			}
			recordUsage(ev.Usage)
		case "message_stop":
			sawStop = true
		}
		if sawStop {
			break
		}
	}
	// A stream that ends without message_stop was cut off mid-reply;
	// fail loudly instead of running a half-accumulated tool call
	// as if it were complete.
	if !sawStop {
		return protocolError("anthropic", "stream ended without message_stop")
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	for _, idx := range order {
		a := accs[idx]
		if !a.done && msg.FinishReason != "max_tokens" {
			return protocolError("anthropic", "content block %d is unfinished", idx)
		}
		if a.kind == "tool_use" && (a.id == "" || a.name == "") {
			return protocolError("anthropic", "tool_use is missing its id or name")
		}
		if a.kind == "tool_use" && a.args.Len() > 0 {
			a.block.Input = json.RawMessage(a.args.String())
		}
		if a.kind == "tool_use" && !json.Valid(a.block.Input) {
			// Truncated tool input is never dispatched or replayed as a block.
			if msg.FinishReason == "max_tokens" {
				continue
			}
			return protocolError("anthropic", "tool_use %q returned invalid JSON input", a.id)
		}
		if a.block.Type != "" {
			msg.AnthropicContent = append(msg.AnthropicContent, a.block)
		}
		if a.kind != "tool_use" || a.name == "" || a.id == "" {
			continue
		}
		args := a.args.String()
		if args == "" {
			args = string(a.block.Input)
			if args == "" {
				args = "{}"
			}
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID: a.id, Type: "function",
			Function: FunctionCall{Name: a.name, Arguments: args},
		})
	}
	return completeStream("anthropic", msg, usage, onDone)
}
