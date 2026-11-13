package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

// Line-edit recovery: the model first fails an oldText edit (stale
// text), then fixes the same line with a startLine edit — in two
// steps, without re-reading the file. This is the fix-loop economy
// the line form exists for: a failed edit costs one step, not a
// read plus a blind retry.
func TestRunLineEditAfterFailedOldText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fix.txt"), []byte("line one\nline two\nline three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolCall := func(id, name string, args map[string]any) string {
		ab, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%s,\"function\":{\"name\":%s,\"arguments\":%s}}]}}]}\n\n",
			strconv.Quote(id), strconv.Quote(name), strconv.Quote(string(ab)))
	}
	textReply := func(text string) string {
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", strconv.Quote(text))
	}
	usage := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			// Stale oldText: the first line is real (line 2), the
			// second is not, so the whole oldText matches 0 times
			// but the diagnostic can still name line 2.
			fmt.Fprint(w, toolCall("call_1", "edit", map[string]any{
				"path": "fix.txt", "oldText": "line two\nline TWO", "newText": "line TWO",
			}))
		case 2:
			// Line-anchored retry, no oldText at all.
			fmt.Fprint(w, toolCall("call_2", "edit", map[string]any{
				"path": "fix.txt", "startLine": 2, "newText": "line TWO",
			}))
		default:
			fmt.Fprint(w, textReply("fixed"))
		}
		fmt.Fprint(w, usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var events []Event
	cfg := &Config{
		Client:   &llm.Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", API: "chat"},
		Exec:     &tools.Executor{Root: root},
		Tools:    ToolDefs(),
		System:   "test system",
		Mode:     "auto",
		MaxSteps: 10,
		OnEvent:  func(e Event) { events = append(events, e) },
	}
	text, _, err := cfg.Run(context.Background(), []llm.Message{{Role: "user", Content: "fix line two"}})
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if text != "fixed" {
		t.Fatalf("final text = %q, want fixed", text)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "fix.txt")); string(got) != "line one\nline TWO\nline three\n" {
		t.Fatalf("file = %q", got)
	}
	// Two edit calls, no read: the recovery cost one step, and the
	// failure diagnostic — not a re-read — is what made the second
	// call possible.
	var edits, reads int
	for _, e := range events {
		if e.Kind == EventToolStart && e.ToolName == "edit" {
			edits++
		}
		if e.Kind == EventToolStart && e.ToolName == "read" {
			reads++
		}
	}
	if edits != 2 {
		t.Fatalf("edit calls = %d, want 2", edits)
	}
	if reads != 0 {
		t.Fatalf("read calls = %d, want 0 (no re-read to recover)", reads)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("LLM calls = %d, want 3", got)
	}
	// The first edit's failure must have named the matching line, so
	// a model reading the observation knows line 2 is the target.
	var sawDiag bool
	for _, e := range events {
		if e.Kind == EventToolEnd && e.ToolName == "edit" && e.ToolErr != nil &&
			strings.Contains(e.ToolOutput, "line(s) 2") {
			sawDiag = true
		}
	}
	if !sawDiag {
		t.Fatal("first edit failure did not name line 2 in its observation")
	}
}
