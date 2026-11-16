package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func protocolEvent(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return "data: " + string(b) + "\n\n"
}

func protocolTerminal(api string) string {
	switch api {
	case "responses":
		return protocolEvent(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{}}})
	case "anthropic":
		return protocolEvent(map[string]any{"type": "message_stop"})
	default:
		return "data: [DONE]\n\n"
	}
}

func protocolToolStream(api string, calls []ToolCall) string {
	var out strings.Builder
	switch api {
	case "responses":
		var items []any
		for _, c := range calls {
			items = append(items, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Function.Name, "arguments": c.Function.Arguments})
		}
		out.WriteString(protocolEvent(map[string]any{"type": "response.completed", "response": map[string]any{"output": items}}))
	case "anthropic":
		for i, c := range calls {
			out.WriteString(protocolEvent(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": map[string]any{}}}))
			out.WriteString(protocolEvent(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.Function.Arguments}}))
			out.WriteString(protocolEvent(map[string]any{"type": "content_block_stop", "index": i}))
		}
		out.WriteString(protocolTerminal(api))
	default:
		for i, c := range calls {
			out.WriteString(protocolEvent(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": c.ID, "type": "function", "function": c.Function}}}}}}))
		}
		out.WriteString(protocolTerminal(api))
	}
	return out.String()
}

func runProtocolStream(t *testing.T, api, payload string) (Message, bool, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, payload) }))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, API: api, APIKey: "test", Model: "test"}
	var msg Message
	done := false
	err := c.Stream(context.Background(), nil, nil, nil, func(m Message, _ Usage) { msg = m; done = true }, nil)
	return msg, done, err
}

func TestCorruptSSEIsNeverCompleted(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		t.Run(api, func(t *testing.T) {
			_, done, err := runProtocolStream(t, api, "data: {BROKEN\n\n"+protocolTerminal(api))
			if err == nil || done {
				t.Fatalf("corrupt stream accepted: done=%v err=%v", done, err)
			}
		})
	}
}

func TestInvalidToolBatchesAreNeverCompleted(t *testing.T) {
	valid := ToolCall{ID: "call", Type: "function", Function: FunctionCall{Name: "read", Arguments: `{"path":"x"}`}}
	for _, api := range []string{"chat", "responses", "anthropic"} {
		for _, kind := range []string{"missing-id", "missing-name", "bad-json", "array", "null", "duplicate-id"} {
			t.Run(api+"/"+kind, func(t *testing.T) {
				call := valid
				switch kind {
				case "missing-id":
					call.ID = ""
				case "missing-name":
					call.Function.Name = ""
				case "bad-json":
					call.Function.Arguments = `{"path":`
				case "array":
					call.Function.Arguments = `[]`
				case "null":
					call.Function.Arguments = `null`
				}
				calls := []ToolCall{call}
				if kind == "duplicate-id" {
					calls = append(calls, valid)
				}
				_, done, err := runProtocolStream(t, api, protocolToolStream(api, calls))
				if err == nil || done {
					t.Fatalf("invalid batch accepted: done=%v err=%v", done, err)
				}
			})
		}
	}
}

func TestSSEFraming(t *testing.T) {
	for _, sep := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmt.Sprintf("%q", sep), func(t *testing.T) {
			payload := strings.Join([]string{"\ufeff: heartbeat", "event: example", "id: 1", "data: {", `data: "text":"中文"}`, "", ": ping", ""}, sep)
			s := newSSEScanner(iotest.OneByteReader(strings.NewReader(payload)), 0, "test", "chat")
			defer s.close()
			data, ok, err := s.next()
			if err != nil || !ok || data != "{\n\"text\":\"中文\"}" {
				t.Fatalf("data=%q ok=%v err=%v", data, ok, err)
			}
			if _, ok, err := s.next(); ok || err != nil {
				t.Fatalf("trailer: %v %v", ok, err)
			}
		})
	}
}

func TestSSEIncompleteAndOversizeEvents(t *testing.T) {
	for _, payload := range []string{"data: {}\n", "data: " + strings.Repeat("x", maxSSEEventBytes) + "\n\n", strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", 4096) + "\n"} {
		s := newSSEScanner(strings.NewReader(payload), 0, "test", "chat")
		_, ok, err := s.next()
		s.close()
		var pe *ProtocolError
		if ok || !errors.As(err, &pe) || !IsRetryable(err) {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	}
}

func TestOutOfOrderToolEventsAreRejected(t *testing.T) {
	fixtures := []struct{ api, data string }{
		{"chat", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`},
		{"chat", `{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"read","arguments":"{}"}}]}}]}`},
		{"responses", `{"type":"response.function_call_arguments.delta","item_id":"x","delta":"{}"}`},
		{"responses", `{"type":"response.function_call_arguments.done","item_id":"x","arguments":"{}"}`},
		{"anthropic", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		{"anthropic", `{"type":"content_block_stop","index":0}`},
		{"anthropic", `{"type":"content_block_start","content_block":{"type":"tool_use","id":"a","name":"read","input":{}}}`},
	}
	for i, f := range fixtures {
		t.Run(fmt.Sprintf("%s/%d", f.api, i), func(t *testing.T) {
			_, done, err := runProtocolStream(t, f.api, "data: "+f.data+"\n\n"+protocolTerminal(f.api))
			var pe *ProtocolError
			if done || !errors.As(err, &pe) {
				t.Fatalf("done=%v err=%v", done, err)
			}
		})
	}
}

func TestUnknownEventsAndMissingTerminals(t *testing.T) {
	for _, api := range []string{"chat", "responses", "anthropic"} {
		unknown := protocolEvent(map[string]any{"type": "future.extension"})
		if _, done, err := runProtocolStream(t, api, unknown+protocolTerminal(api)); !done || err != nil {
			t.Fatalf("%s unknown event: %v %v", api, done, err)
		}
		_, done, err := runProtocolStream(t, api, unknown)
		var pe *ProtocolError
		if done || !errors.As(err, &pe) {
			t.Fatalf("%s missing terminal: %v %v", api, done, err)
		}
	}
}

func TestToolStreamIdentityAndTerminalConflicts(t *testing.T) {
	chat := func(id, name, args string) string {
		return protocolEvent(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": id, "function": map[string]any{"name": name, "arguments": args}}}}}}})
	}
	added := protocolEvent(map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": "item", "type": "function_call", "call_id": "a", "name": "read", "arguments": ""}})
	delta := protocolEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "item", "delta": `{"path":"x"}`})
	argsDone := protocolEvent(map[string]any{"type": "response.function_call_arguments.done", "item_id": "item", "arguments": `{"path":"x"}`})
	start := protocolEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "a", "name": "read", "input": map[string]any{}}})
	stop := protocolEvent(map[string]any{"type": "content_block_stop", "index": 0})
	fixtures := []struct{ api, body string }{
		{"chat", chat("a", "read", `{"path":"x"}`) + chat("b", "read", "")},
		{"chat", chat("a", "read", `{"path":"x"}`) + chat("a", "write", "")},
		{"chat", protocolEvent(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop"}}}) + chat("a", "read", `{}`)},
		{"responses", added + added},
		{"responses", added + delta + argsDone + delta},
		{"responses", added + delta + protocolEvent(map[string]any{"type": "response.function_call_arguments.done", "item_id": "item", "arguments": `{"path":"changed"}`})},
		{"anthropic", start + start},
		{"anthropic", start + stop + stop},
		{"anthropic", start + stop + protocolEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}})},
		{"anthropic", start},
	}
	for i, f := range fixtures {
		t.Run(fmt.Sprintf("%s/%d", f.api, i), func(t *testing.T) {
			_, done, err := runProtocolStream(t, f.api, f.body+protocolTerminal(f.api))
			var pe *ProtocolError
			if done || !errors.As(err, &pe) {
				t.Fatalf("done=%v err=%v", done, err)
			}
		})
	}
}

func TestTruncatedToolCallsNeverBecomeActions(t *testing.T) {
	chatCall := strings.TrimSuffix(protocolToolStream("chat", []ToolCall{{ID: "a", Function: FunctionCall{Name: "read", Arguments: `{"path":`}}}), protocolTerminal("chat"))
	fixtures := []struct{ api, body string }{
		{"chat", chatCall + protocolEvent(map[string]any{"choices": []any{map[string]any{"finish_reason": "length"}}})},
		{"responses", protocolEvent(map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": "item", "type": "function_call", "call_id": "a", "name": "read", "arguments": `{"path":`}}) + protocolEvent(map[string]any{"type": "response.incomplete"})},
		{"anthropic", protocolEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "a", "name": "read", "input": map[string]any{}}}) + protocolEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "max_tokens"}})},
	}
	for _, f := range fixtures {
		t.Run(f.api, func(t *testing.T) {
			m, done, err := runProtocolStream(t, f.api, f.body+protocolTerminal(f.api))
			if !done || err != nil || len(m.ToolCalls) != 0 || len(m.AnthropicContent) != 0 {
				t.Fatalf("done=%v err=%v m=%+v", done, err, m)
			}
		})
	}
}
