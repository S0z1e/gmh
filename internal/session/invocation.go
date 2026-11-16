package session

import (
	"encoding/json"
	"fmt"

	"minimalharness/internal/llm"
)

const invocationMarkerRole = "__tool_invocation__"

// RecordInvocation commits the final arguments before approval or execution.
// It never replaces the original assistant record on disk.
func (s *Store) RecordInvocation(inv llm.ToolInvocation) error {
	return s.Append(llm.Message{Role: invocationMarkerRole, ToolInvocations: []llm.ToolInvocation{inv}})
}

func applyInvocation(msgs []llm.Message, records []llm.ToolInvocation) error {
	if len(records) != 1 {
		return fmt.Errorf("invalid tool invocation record")
	}
	inv := records[0]
	var assistant *llm.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			assistant = &msgs[i]
			break
		}
		if msgs[i].Role == "user" && IsTurnStart(msgs[i]) {
			break
		}
	}
	if assistant == nil || inv.CallIndex < 0 || inv.CallIndex >= len(assistant.ToolCalls) {
		return fmt.Errorf("tool invocation has no matching assistant call")
	}
	call := &assistant.ToolCalls[inv.CallIndex]
	if inv.ID == "" || inv.ToolCallID != call.ID || inv.ToolName != call.Function.Name || inv.OriginalArguments != call.Function.Arguments || !json.Valid([]byte(inv.Arguments)) {
		return fmt.Errorf("tool invocation does not match its original request")
	}
	for _, previous := range assistant.ToolInvocations {
		if previous.CallIndex == inv.CallIndex || previous.ID == inv.ID {
			return fmt.Errorf("duplicate tool invocation record")
		}
	}
	call.Function.Arguments = inv.Arguments
	assistant.ToolInvocations = append(assistant.ToolInvocations, inv)
	return nil
}
