package session

import (
	"testing"

	"minimalharness/internal/llm"
)

func TestClearDropsEarlierMessagesOnLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := New("test-clear")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Append(llm.Message{Role: "user", Content: "before clear"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(llm.Message{Role: "assistant", Content: "old answer"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(llm.Message{Role: "user", Content: "after clear"}); err != nil {
		t.Fatal(err)
	}
	msgs, err := Load("test-clear")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "after clear" {
		t.Fatalf("Load after clear = %+v, want only the post-clear message", msgs)
	}
}

func TestLoadWithoutClearReturnsAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := New("test-noclear")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.Append(llm.Message{Role: "user", Content: "one"})
	_ = store.Append(llm.Message{Role: "assistant", Content: "two"})
	msgs, err := Load("test-noclear")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Load = %d messages, want 2", len(msgs))
	}
}

func TestClearThenDanglingToolCallDropped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := New("test-clear-dangling")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.Append(llm.Message{Role: "user", Content: "before"})
	_ = store.Clear()
	_ = store.Append(llm.Message{Role: "user", Content: "after"})
	_ = store.Append(llm.Message{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "read"}}}})
	msgs, err := Load("test-clear-dangling")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "after" {
		t.Fatalf("Load = %+v, want only 'after'", msgs)
	}
}
