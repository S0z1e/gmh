package agent

import (
	"strings"
	"testing"

	"minimalharness/internal/llm"
)

func toolMsg(content string) llm.Message {
	return llm.Message{Role: "tool", Content: content, ToolCallID: "1"}
}

func TestCapTextKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
	out := capText(s, 40)
	if len(out) > 120 {
		t.Fatalf("capText too long: %d chars", len(out))
	}
	if !strings.HasPrefix(out, strings.Repeat("a", 30)) {
		t.Fatalf("head not kept:\n%s", out)
	}
	if !strings.HasSuffix(out, strings.Repeat("b", 10)) {
		t.Fatalf("tail not kept:\n%s", out)
	}
	if !strings.Contains(out, "[truncated: 40 chars shown of 206]") {
		t.Fatalf("marker missing:\n%s", out)
	}
}

func TestCapTextLeavesShortUntouched(t *testing.T) {
	if got := capText("short", 100); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
}

func TestTruncateToolResultsUniformCap(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		toolMsg(big), // old
		toolMsg(big), // old
		toolMsg(big), // recent
	}
	out := truncateToolResults(msgs, 32*1024)
	if out[0].Content != "sys" || out[1].Content != "do it" {
		t.Fatal("non-tool messages must be untouched")
	}
	// One cap for old and new alike: nothing over the cap is sent
	// whole, and nothing under it is touched.
	for _, i := range []int{2, 3, 4} {
		if len(out[i].Content) != 5000 {
			t.Fatalf("tool result %d changed under the cap: %d chars", i, len(out[i].Content))
		}
	}
	// Input must not be mutated.
	if len(msgs[2].Content) != 5000 {
		t.Fatal("truncateToolResults mutated its input")
	}
}

func TestTruncateToolResultsCapsLarge(t *testing.T) {
	big := strings.Repeat("x", 40000)
	msgs := []llm.Message{toolMsg(big)}
	out := truncateToolResults(msgs, 32*1024)
	if len(out[0].Content) > 33*1024 {
		t.Fatalf("tool result not capped: %d chars", len(out[0].Content))
	}
	if !strings.Contains(out[0].Content, "[truncated:") {
		t.Fatal("truncation marker missing")
	}
}

// sameMessage compares the fields these tests care about.
// (llm.Message holds slices, so == does not compile.)
func sameMessage(a, b llm.Message) bool {
	return a.Role == b.Role && a.Content == b.Content &&
		a.ToolCallID == b.ToolCallID &&
		len(a.ToolCalls) == len(b.ToolCalls) &&
		len(a.Images) == len(b.Images)
}

// sentView applies the same truncate + prune pipeline the loop sends.
func sentView(msgs []llm.Message) []llm.Message {
	out := truncateToolResults(msgs, 32*1024)
	return pruneOldToolResults(out, pruneThresholdChars, pruneBlockChars, pruneKeepNewestBlocks)
}

func TestPruneDoesNothingUnderThreshold(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		toolMsg(strings.Repeat("a", 5000)),
		toolMsg(strings.Repeat("b", 5000)),
	}
	out := sentView(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("view length changed: %d -> %d", len(msgs), len(out))
	}
	for i := range msgs {
		if !sameMessage(out[i], msgs[i]) {
			t.Fatalf("message %d changed under the threshold", i)
		}
	}
}

// TestPruneKeepsNewestBlockAndUserMessages: with 10 x 5,000-char tool
// results (50,000 total, over the 48 KiB threshold) in 24 KiB blocks,
// blocks are [0-3], [4-7], [8-9]; only the newest block survives.
func TestPruneKeepsNewestBlockAndUserMessages(t *testing.T) {
	msgs := []llm.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, llm.Message{Role: "user", Content: "step"})
		msgs = append(msgs, llm.Message{Role: "tool", Content: strings.Repeat(string(rune('a'+i)), 5000), ToolCallID: "call-" + string(rune('0'+i))})
	}
	out := sentView(msgs)
	for i, m := range msgs {
		got := out[i]
		if m.Role != "tool" {
			if !sameMessage(got, m) {
				t.Fatalf("non-tool message %d changed", i)
			}
			continue
		}
		// Tool messages are at odd indices (1 user + 1 tool per step,
		// after the leading system message): tool k is at index 2k+2.
		k := (i - 2) / 2
		if k < 8 {
			if got.Content != prunedToolResultPlaceholder {
				t.Fatalf("old tool message %d (block %d) not pruned: %d chars", i, k/4, len(got.Content))
			}
			if got.Role != "tool" || got.ToolCallID != m.ToolCallID {
				t.Fatalf("pruned message %d lost role/call id: %+v", i, got)
			}
		} else {
			if !sameMessage(got, m) {
				t.Fatalf("newest-block tool message %d changed", i)
			}
		}
	}
}

func TestPruneDoesNotMutateInput(t *testing.T) {
	big := strings.Repeat("x", 60000)
	msgs := []llm.Message{toolMsg(big), toolMsg(big)}
	before := []string{msgs[0].Content, msgs[1].Content}
	_ = sentView(msgs)
	for i, m := range msgs {
		if m.Content != before[i] {
			t.Fatalf("input message %d mutated", i)
		}
	}
}

func TestPruneBoundsPrompt(t *testing.T) {
	msgs := []llm.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 40; i++ {
		msgs = append(msgs, toolMsg(strings.Repeat("x", 5000)))
	}
	out := sentView(msgs)
	total := 0
	for _, m := range out {
		if m.Role == "tool" {
			total += len(m.Content)
		}
	}
	if total >= 64*1024 {
		t.Fatalf("pruned sent view not bounded: %d tool chars for 200,000 input", total)
	}
	if !sameMessage(out[len(out)-1], msgs[len(msgs)-1]) {
		t.Fatal("newest tool result must survive pruning intact")
	}
}

// isPrefixExtension reports whether cur starts with prev, message for
// message, so the sent view only grew by appending.
func isPrefixExtension(prev, cur []llm.Message) bool {
	if len(cur) < len(prev) {
		return false
	}
	for i := range prev {
		if !sameMessage(cur[i], prev[i]) {
			return false
		}
	}
	return true
}

// diffIsPruneOnly reports whether cur differs from prev only in that
// some tool messages became the fixed placeholder (plus appended
// messages at the end).
func diffIsPruneOnly(prev, cur []llm.Message) bool {
	if len(cur) < len(prev) {
		return false
	}
	for i := range prev {
		if sameMessage(cur[i], prev[i]) {
			continue
		}
		if prev[i].Role != "tool" || cur[i].Role != "tool" ||
			cur[i].ToolCallID != prev[i].ToolCallID ||
			cur[i].Content != prunedToolResultPlaceholder {
			return false
		}
	}
	return true
}

// TestPruneSentFormIsStableBetweenPrunes is the prefix-cache property
// for pruning: appending history may only extend the sent view, or
// change it by placeholdering old tool results — never by rewriting
// a message that was already sent. A prune transition costs one
// cold step; every step after it must be a pure extension again.
func TestPruneSentFormIsStableBetweenPrunes(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
	}
	prev := sentView(msgs)
	prunes := 0
	for step := 0; step < 30; step++ {
		msgs = append(msgs, toolMsg(strings.Repeat(string(rune('a'+step%26)), 5000)))
		cur := sentView(msgs)
		if isPrefixExtension(prev, cur) {
			// No prune this step: pure append.
		} else if diffIsPruneOnly(prev, cur) {
			prunes++
		} else {
			t.Fatalf("step %d: sent view changed in a way that is neither append nor prune", step)
		}
		prev = cur
	}
	if prunes == 0 {
		t.Fatal("test never triggered a prune; threshold not crossed")
	}
}

// TestTruncateSentFormIsStable is the prefix-cache property: once a
// tool result has been sent, appending newer results must not change
// its sent form. The old sliding window (newest 2 full, older cut to
// 2,000 chars) broke this on every step.
func TestTruncateSentFormIsStable(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		toolMsg(big),
	}
	first := truncateToolResults(msgs, 32*1024)
	// Three more steps append more tool results (one over the cap).
	msgs = append(msgs,
		toolMsg(strings.Repeat("y", 40000)),
		toolMsg("small"),
		toolMsg(strings.Repeat("z", 3000)),
	)
	later := truncateToolResults(msgs, 32*1024)
	if later[1].Content != first[1].Content {
		t.Fatalf("already-sent tool result changed:\nbefore %d chars\n after %d chars", len(first[1].Content), len(later[1].Content))
	}
	if len(later[2].Content) > 33*1024 || !strings.Contains(later[2].Content, "[truncated:") {
		t.Fatalf("over-cap result not truncated the same way: %d chars", len(later[2].Content))
	}
}
