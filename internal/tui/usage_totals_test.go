package tui

import (
	"strings"
	"testing"
)

func TestUsageRestoredBeforeFirstInput(t *testing.T) {
	m := NewModel("resumed").WithMeta("auto", "chat", "model").WithSessionTotals(12345, 0)
	if got := m.metaLine(); !strings.Contains(got, "12,345 tokens") {
		t.Fatalf("initial status omitted restored usage: %s", got)
	}
}
