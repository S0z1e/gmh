package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"minimalharness/internal/llm"
)

func TestInvocationReplayScopesIDsToTheirAssistantBatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("invocations")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"first", "second"} {
		original := `{"path":"requested"}`
		actual := `{"path":"` + name + `"}`
		if err := s.Append(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "reused", Function: llm.FunctionCall{Name: "read", Arguments: original}}}}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordInvocation(llm.ToolInvocation{ID: name, CallIndex: 0, ToolCallID: "reused", ToolName: "read", OriginalArguments: original, Arguments: actual}); err != nil {
			t.Fatal(err)
		}
		if name == "first" {
			if err := s.Append(llm.Message{Role: "tool", ToolCallID: "reused", InvocationID: name, Content: "done"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	msgs, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[0].ToolCalls[0].Function.Arguments != `{"path":"first"}` || msgs[2].ToolCalls[0].Function.Arguments != `{"path":"second"}` {
		t.Fatalf("wrong batch rewritten: %+v", msgs)
	}
	if msgs[1].InvocationID != "first" || msgs[3].InvocationID != "second" || msgs[3].Kind != "interrupted" {
		t.Fatalf("lost result identity: %+v", msgs)
	}
	if err := s.MarkCompactMessages(msgs); err != nil {
		t.Fatal(err)
	}
	again, err := Load(s.ID)
	if err != nil || len(again) != 4 || again[2].ToolInvocations[0].OriginalArguments != `{"path":"requested"}` {
		t.Fatalf("checkpoint lost provenance: %+v %v", again, err)
	}
}

func TestInvocationReplayRejectsUnboundRecords(t *testing.T) {
	for _, change := range []string{"missing", "index", "call_id", "name", "original", "json", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			s, err := New("invalid")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if change != "missing" {
				if err := s.Append(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"before"}`}}}}); err != nil {
					t.Fatal(err)
				}
			}
			inv := llm.ToolInvocation{ID: "invocation", ToolCallID: "call", ToolName: "read", OriginalArguments: `{"path":"before"}`, Arguments: `{"path":"after"}`}
			switch change {
			case "index":
				inv.CallIndex = 1
			case "call_id":
				inv.ToolCallID = "other"
			case "name":
				inv.ToolName = "write"
			case "original":
				inv.OriginalArguments = `{}`
			case "json":
				inv.Arguments = `{`
			}
			if err := s.RecordInvocation(inv); err != nil {
				t.Fatal(err)
			}
			if change == "duplicate" {
				if err := s.RecordInvocation(inv); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Load(s.ID); err == nil {
				t.Fatal("unbound invocation silently replayed")
			}
		})
	}
}

func TestInvocationMarkerKeepsOriginalRecordOnDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("original")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	original := `{"path":"before"}`
	if err := s.Append(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Function: llm.FunctionCall{Name: "read", Arguments: original}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordInvocation(llm.ToolInvocation{ID: "inv", ToolCallID: "call", ToolName: "read", OriginalArguments: original, Arguments: `{"path":"after"}`}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	var raw llm.Message
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 3)[1]), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.ToolCalls[0].Function.Arguments != original || len(raw.ToolInvocations) != 0 {
		t.Fatalf("mutated raw request: %+v", raw)
	}
}
