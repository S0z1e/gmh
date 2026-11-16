package agent

import (
	"strings"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

func TestApprovalReceivesCompleteInvocation(t *testing.T) {
	args := `{"path":"x.txt","content":"` + strings.Repeat("中", 300) + `TAIL?"}`
	var got ApprovalRequest
	cfg := &Config{Client: &llm.Client{APIKey: "fake", Model: "test"}, Exec: &tools.Executor{Root: t.TempDir()}, Tools: ToolDefs(), Mode: "suggest", MaxSteps: 3, Ask: func(req ApprovalRequest) (bool, error) { got = req; return true, nil }}
	runTwoTurns(t, cfg, toolCallSSE("c1", "write", args))
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
		runTwoTurns(t, cfg, toolCallSSE("a", tool, `{"url":"https://example.com"}`))
		if !asked {
			t.Fatalf("suggest mode bypassed approval for %s", tool)
		}
	}
}
