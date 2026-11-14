package session

import (
	"os"
	"strings"
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

// Audit records land in a sidecar file, not in the transcript.
func TestAuditSidecar(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New("audit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Append(llm.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(map[string]any{"kind": "approval", "tool": "write", "decision": "denied"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"decision":"denied"`) {
		t.Fatalf("audit sidecar = %s", data)
	}
	// The transcript itself has no audit record.
	msgs, err := Load("audit-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatalf("transcript = %+v", msgs)
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

func TestMarkCompactKeepsOnlySummaryOnLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := New("test-compact")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.Append(llm.Message{Role: "user", Content: "old task"})
	_ = store.Append(llm.Message{Role: "assistant", Content: "old answer"})
	if err := store.MarkCompact("1. Task summary — did the old task"); err != nil {
		t.Fatal(err)
	}
	_ = store.Append(llm.Message{Role: "user", Content: "new task"})
	msgs, err := Load("test-compact")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Load = %d messages, want 2 (summary + new task)", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "1. Task summary") {
		t.Fatalf("first message should be the summary, got: %q", msgs[0].Content)
	}
	if msgs[1].Content != "new task" {
		t.Fatalf("second message = %q, want 'new task'", msgs[1].Content)
	}
}

func TestArchiveCopyKeepsOriginal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := New("test-archive")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.Append(llm.Message{Role: "user", Content: "original"})
	// Flush by closing, then reopen through ArchiveCopy's read.
	store.Close()
	path, err := store.ArchiveCopy("test-archive-pre-compact-000000")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "original") {
		t.Fatalf("archive missing original content: %s", data)
	}
}

func TestClearThenDanglingToolCallRepaired(t *testing.T) {
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
	if len(msgs) != 3 || msgs[0].Content != "after" || msgs[2].ToolCallID != "1" || !msgs[2].ToolError {
		t.Fatalf("Load = %+v, want post-clear history with an interrupted result", msgs)
	}
}

// An image message round-trips through the file with its path and
// media type, but the bytes never land in the file.
func TestImageMessageStoresPathOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := New("test-image")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	msg := llm.Message{
		Role:    "user",
		Content: "Image attached: pic.png (2x2, 100 bytes).",
		Images:  []llm.Image{{Path: "pic.png", MediaType: "image/png", Data: "aGVsbG8=", Width: 2, Height: 2, Bytes: 100}},
	}
	if err := store.Append(msg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "aGVsbG8=") {
		t.Fatal("session file must not carry image bytes")
	}
	msgs, err := Load("test-image")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].Images) != 1 {
		t.Fatalf("loaded = %+v", msgs)
	}
	img := msgs[0].Images[0]
	if img.Path != "pic.png" || img.MediaType != "image/png" || img.Data != "" || img.Width != 2 {
		t.Fatalf("loaded image = %+v", img)
	}
}
