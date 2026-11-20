package rpc

import (
	"bytes"
	"encoding/json"
	"testing"

	"minimalharness/internal/agent"
)

func TestToolEventsPreserveInvocationIdentity(t *testing.T) {
	for _, kind := range []agent.EventKind{agent.EventToolStart, agent.EventToolEnd, agent.EventToolBackgrounded, agent.EventToolBackgroundDone} {
		var output bytes.Buffer
		s := &Server{out: &output}
		s.onEvent(agent.Event{Kind: kind, ToolName: "bash", ToolArgs: `{"command":"echo hi"}`, ToolCallID: "call", InvocationID: "invocation", JobID: "job"})
		var event map[string]any
		if err := json.Unmarshal(output.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["tool_call_id"] != "call" || event["invocation_id"] != "invocation" {
			t.Fatalf("%s lost identity: %s", kind, output.String())
		}
	}
}
