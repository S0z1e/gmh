package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"minimalharness/internal/llm"
)

// A turn with thinking reaches the GUI as flat thinking_delta events
// in stream order, then one thinking_end carrying the full text, and
// agent_settled.text stays the answer only.
func TestThinkingEventsOverRPC(t *testing.T) {
	a, runner, _ := setupTestApp(t, "auto", []fakeStep{
		{kind: "text", text: "Done.", thinking: "Let me think."},
	})
	d := startDriver(t, a, runner)
	d.waitFor("ready", 5*time.Second)
	d.send(map[string]any{"id": "p1", "type": "prompt", "message": "hi"})
	d.waitFor("response", 5*time.Second)
	d.waitFor("turn_started", 5*time.Second)

	var deltas []string
	for i := 0; i < 3; i++ {
		ev := d.waitFor("thinking_delta", 5*time.Second)
		text, _ := ev["text"].(string)
		deltas = append(deltas, text)
	}
	if got := strings.Join(deltas, ""); strings.TrimSpace(got) != "Let me think." {
		t.Fatalf("thinking deltas = %q, want them to join to %q", deltas, "Let me think.")
	}
	end := d.waitFor("thinking_end", 5*time.Second)
	if text, _ := end["text"].(string); strings.TrimSpace(text) != "Let me think." {
		t.Fatalf("thinking_end text = %q, want the full thinking", text)
	}
	settled := d.waitFor("agent_settled", 10*time.Second)
	if settled["error"] != nil {
		t.Fatalf("settled with error: %v", settled["error"])
	}
	text, _ := settled["text"].(string)
	if !strings.Contains(text, "Done.") {
		t.Fatalf("settled text = %q, want it to contain Done.", text)
	}
	if strings.Contains(text, "think") {
		t.Fatalf("settled text = %q, must not contain thinking", text)
	}
	// The fake provider reports no reasoning tokens, so the usage
	// object must not carry a reasoning field at all.
	usage, _ := settled["usage"].(map[string]any)
	if _, ok := usage["reasoning"]; ok {
		t.Fatalf("settled usage = %v, want no reasoning field when the provider reports none", usage)
	}
	d.quit(t)
}

// usageJSON surfaces reasoning tokens when the provider reported
// them, and omits the field when it did not. Reasoning is a subset
// of completion, so it never changes the totals.
func TestUsageJSONReasoning(t *testing.T) {
	with := usageJSON(llm.Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18, ReasoningTokens: 5})
	b, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["reasoning"] != float64(5) || m["completion"] != float64(8) || m["total"] != float64(18) {
		t.Fatalf("usage JSON = %s, want reasoning 5, completion 8, total 18", b)
	}
	without := usageJSON(llm.Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18})
	b, err = json.Marshal(without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "reasoning") {
		t.Fatalf("usage JSON = %s, want no reasoning field when zero", b)
	}
}
