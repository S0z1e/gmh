package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

func TestApprovalReceivesCompleteInvocation(t *testing.T) {
	args := `{"path":"x.txt","content":"` + strings.Repeat("中", 300) + `TAIL?"}`
	var got ApprovalRequest
	cfg := &Config{Client: &llm.Client{APIKey: "fake", Model: "test"}, Exec: &tools.Executor{Root: t.TempDir()}, Tools: ToolDefs(), Mode: "suggest", MaxSteps: 3, Ask: func(req ApprovalRequest) (bool, error) { got = req; return true, nil }}
	chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "write", Arguments: args}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	runTwoTurns(t, cfg, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n")
	if got.ToolName != "write" || got.Arguments != args {
		t.Fatalf("approval did not receive full invocation: %+v", got)
	}
}

func TestSuggestApprovesNetworkAndPluginTools(t *testing.T) {
	for _, tool := range []string{"webfetch", "external_plugin"} {
		asked := false
		defs := append(ToolDefs(), llm.Tool("external_plugin", "external tool", `{"type":"object"}`))
		cfg := &Config{Client: &llm.Client{APIKey: "fake", Model: "test"}, Exec: &tools.Executor{Root: t.TempDir()}, Tools: defs, Mode: "suggest", MaxSteps: 3, Ask: func(req ApprovalRequest) (bool, error) {
			asked = true
			if req.ToolName != tool {
				t.Error("wrong approval")
			}
			return false, nil
		}}
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []llm.ToolCall{{ID: "a", Type: "function", Function: llm.FunctionCall{Name: tool, Arguments: `{"url":"https://example.com"}`}}}}}}})
		runTwoTurns(t, cfg, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n")
		if !asked {
			t.Fatalf("suggest mode bypassed approval for %s", tool)
		}
	}
}
