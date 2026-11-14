package session

import (
	"minimalharness/internal/llm"
	"testing"
)

func TestLoadRepairsInterruptedBatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("repair")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Append(llm.Message{Role: "user", Content: "task"})
	s.Append(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "a"}, {ID: "b"}}})
	s.Append(llm.Message{Role: "tool", ToolCallID: "a", Content: "done"})
	s.Append(llm.Message{Role: "user", Content: "continue"})
	msgs, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 || msgs[3].Role != "tool" || msgs[3].ToolCallID != "b" || !msgs[3].ToolError {
		t.Fatalf("history not repaired: %+v", msgs)
	}
	again, _ := Load(s.ID)
	if len(again) != len(msgs) {
		t.Fatal("repair not stable")
	}
}
func TestLoadRepairsDanglingCallBeforeNextTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := New("dangling")
	defer s.Close()
	s.Append(llm.Message{Role: "user", Content: "task"})
	s.Append(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "a"}}})
	first, _ := Load(s.ID)
	if len(first) != 3 || first[2].ToolCallID != "a" {
		t.Fatalf("%+v", first)
	}
	s.Append(llm.Message{Role: "user", Content: "continue"})
	next, _ := Load(s.ID)
	if len(next) != 4 || next[2].ToolCallID != "a" {
		t.Fatalf("%+v", next)
	}
}

func TestExplicitTaskKindAvoidsPrefixAndImageHeuristics(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Kind: "task", Content: "Background job analysis", Images: []llm.Image{{Path: "user.png"}}},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Kind: "hook", Content: "A stop hook requested another step"},
		{Role: "assistant", Content: "final answer"},
	}
	kept, dropped := Rollback(msgs, 1)
	if len(kept) != 0 || dropped != 4 {
		t.Fatalf("kept=%+v dropped=%d", kept, dropped)
	}
}

func TestTruncatedToolArgumentsAreNotReplayedOnResume(t *testing.T) {
	msgs := RepairToolBatches([]llm.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", FinishReason: "length", ToolCalls: []llm.ToolCall{{ID: "partial", Function: llm.FunctionCall{Name: "write", Arguments: `{"path":`}}}},
	})
	if len(msgs) != 2 || len(msgs[1].ToolCalls) != 0 || msgs[1].Content == "" {
		t.Fatalf("invalid arguments survived recovery: %+v", msgs)
	}
}
