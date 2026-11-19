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

func TestUsageUnavailableHidesStaleTotals(t *testing.T) {
	m := NewModel("").WithMeta("auto", "chat", "model").WithSessionTotals(12345, 1)
	updated, _ := m.Update(MetaUpdateMsg{Tokens: 12345, Turns: 2, UsageUnavailable: true})
	got := updated.(Model).metaLine()
	if strings.Contains(got, "12,345") || !strings.Contains(got, "unavailable tokens") {
		t.Fatal(got)
	}
}

func TestUsageCoverageDistinguishesUnknownFromExplicitZero(t *testing.T) {
	m := NewModel("").WithMeta("auto", "chat", "model").WithSessionTotals(0, 1).WithUsageCoverage(false)
	if got := m.metaLine(); !strings.Contains(got, "unknown tokens") || strings.Contains(got, "0 tokens") {
		t.Fatal(got)
	}
	m = m.WithSessionTotals(123, 1)
	if got := m.metaLine(); !strings.Contains(got, "123+ unknown tokens") {
		t.Fatal(got)
	}
	m = m.WithSessionTotals(0, 1).WithUsageCoverage(true)
	if got := m.metaLine(); !strings.Contains(got, "0 tokens") || strings.Contains(got, "unknown") {
		t.Fatal(got)
	}
}
