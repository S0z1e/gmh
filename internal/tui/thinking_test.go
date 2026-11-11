package tui

import (
	"strings"
	"testing"

	"minimalharness/internal/agent"
)

// The first thinking delta replaces the "Thinking…" spinner row with
// the accumulating thinking text, dim and italic, above the live
// answer. After Done, the thinking waits with the reply and both
// flush together on the next print.
func TestModelThinkingReplacesSpinner(t *testing.T) {
	m := NewModel("")
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventThinkingDelta, Text: "Let me "}})
	if got := stripANSI(m.View().Content); !strings.Contains(got, "Let me ") {
		t.Fatalf("live region = %q, want it to contain the thinking", got)
	}
	if got := stripANSI(m.View().Content); strings.Contains(got, "Thinking…") {
		t.Fatalf("live region still shows the spinner after the first thinking delta: %q", got)
	}
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventThinkingDelta, Text: "think."}})
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "answer"}})
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "Let me think.") || !strings.Contains(view, "answer") {
		t.Fatalf("live region = %q, want thinking and answer", view)
	}
	// Thinking streams first, so it sits above the answer.
	if strings.Index(view, "Let me think.") > strings.Index(view, "answer") {
		t.Fatalf("live region = %q, want thinking above the answer", view)
	}
	// thinking_end replaces the accumulated deltas with the full text.
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventThinkingEnd, Text: "Let me think."}})
	if got := stripANSI(m.View().Content); !strings.Contains(got, "Let me think.") {
		t.Fatalf("live region after thinking_end = %q", got)
	}
	next, cmd := m.Update(EventMsg{Event: agent.Event{Kind: agent.EventDone, Text: "answer"}})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("Done should not print on its own; the reply waits for the next print")
	}
	if m.pendingThinking != "Let me think." {
		t.Fatalf("pendingThinking = %q, want the full thinking", m.pendingThinking)
	}
	if m.pendingReply != "answer" {
		t.Fatalf("pendingReply = %q, want answer", m.pendingReply)
	}
	if got := stripANSI(m.View().Content); strings.Contains(got, "Let me think.") || strings.Contains(got, "answer") {
		t.Fatalf("live region should be cleared after Done, got %q", got)
	}
	// The next print flushes thinking and reply together, and both
	// clear.
	next, cmd = m.Update(PrintMsg{Text: "[usage] turn: prompt=1"})
	if cmd == nil {
		t.Fatal("PrintMsg should return a print command")
	}
	if next.(Model).pendingThinking != "" || next.(Model).pendingReply != "" {
		t.Fatal("pendingThinking and pendingReply should clear after the print")
	}
}

// A turn with no thinking keeps the old spinner behaviour: nothing
// to replace, no thinking held after Done.
func TestModelNoThinkingKeepsSpinner(t *testing.T) {
	m := NewModel("")
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "answer"}})
	if m.liveThinking != "" {
		t.Fatalf("liveThinking = %q, want empty", m.liveThinking)
	}
	next, _ := m.Update(EventMsg{Event: agent.Event{Kind: agent.EventDone, Text: "answer"}})
	if next.(Model).pendingThinking != "" {
		t.Fatalf("pendingThinking = %q, want empty", next.(Model).pendingThinking)
	}
}

// renderThinking is plain dim/italic text under the same gutter as
// the answer — no markdown re-styling of the monologue.
func TestRenderThinking(t *testing.T) {
	got := stripANSI(renderThinking("line one\nline two"))
	if !strings.Contains(got, "   line one\n   line two") {
		t.Fatalf("renderThinking = %q, want both lines under the gutter", got)
	}
}
