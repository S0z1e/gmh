package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInvocationReplayUsesEffectiveArgumentsWithoutLeakingProvenance(t *testing.T) {
	args := `{"path":"actual"}`
	history := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Type: "function", Function: FunctionCall{Name: "read", Arguments: args}}}, ToolInvocations: []ToolInvocation{{ID: "invocation", ToolCallID: "call", ToolName: "read", OriginalArguments: `{"path":"requested"}`, Arguments: args}}, AnthropicContent: []AnthropicContentBlock{{Type: "tool_use", ID: "call", Name: "read", Input: json.RawMessage(`{"path":"requested"}`)}}},
		{Role: "tool", ToolCallID: "call", InvocationID: "invocation", Content: "done"},
	}
	_, responses := toResponsesInput(history)
	_, anthropic := toAnthropicMessages(history)
	for name, wire := range map[string]any{"chat": toChatWireMessages(history), "responses": responses, "anthropic": anthropic} {
		data, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "actual") || strings.Contains(string(data), "requested") || strings.Contains(string(data), "invocation") {
			t.Fatalf("%s replay diverged or leaked provenance: %s", name, data)
		}
	}
}
