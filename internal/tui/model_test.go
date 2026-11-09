package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"minimalharness/internal/agent"
)

func apply(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return nm
}

func TestModelStreamsTextIntoLiveRegion(t *testing.T) {
	m := NewModel("")
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "hello "}})
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "world"}})
	if got := m.View().Content; !strings.Contains(got, "hello world") {
		t.Fatalf("live region = %q, want it to contain streamed text", got)
	}
	if !m.working {
		t.Fatal("model should be working while streaming")
	}
}

func TestModelFlushesLiveOnDone(t *testing.T) {
	m := NewModel("")
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "done text"}})
	next, cmd := m.Update(EventMsg{Event: agent.Event{Kind: agent.EventDone, Text: "done text"}})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("Done should return a tea.Println command to commit the text to scrollback")
	}
	if m.working {
		t.Fatal("model should be idle after Done")
	}
	if got := m.View().Content; strings.Contains(got, "done text") {
		t.Fatalf("live region should be cleared after Done, got %q", got)
	}
}

func TestModelShowsCurrentTool(t *testing.T) {
	m := NewModel("")
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventToolStart, ToolName: "grep", ToolArgs: `{"pattern":"TODO"}`}})
	if got := m.View().Content; !strings.Contains(got, "grep") {
		t.Fatalf("live region should show the running tool, got %q", got)
	}
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventToolEnd, ToolName: "grep"}})
	if got := m.View().Content; strings.Contains(got, "grep") {
		t.Fatalf("tool should clear after ToolEnd, got %q", got)
	}
}

func TestModelEnterSubmitsAndClearsInput(t *testing.T) {
	var submitted []string
	m := NewModel("")
	m.OnSubmit = func(s string) { submitted = append(submitted, s) }
	for _, r := range "hi" {
		m = apply(t, m, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	if got := m.View().Content; !strings.Contains(got, "> hi") {
		t.Fatalf("input line = %q, want '> hi'", got)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(submitted) != 1 || submitted[0] != "hi" {
		t.Fatalf("submitted = %v, want [hi]", submitted)
	}
	if got := m.View().Content; strings.Contains(got, "> hi") {
		t.Fatalf("input should clear after Enter, got %q", got)
	}
}

func TestModelBackspaceAndCursor(t *testing.T) {
	m := NewModel("")
	for _, r := range "ab" {
		m = apply(t, m, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.View().Content; !strings.Contains(got, "> a") || strings.Contains(got, "> ab") {
		t.Fatalf("after backspace input = %q, want '> a'", got)
	}
}

func TestModelApprovalDialogReplacesInput(t *testing.T) {
	req := &ApprovalRequest{Prompt: `Run write {"path":"x.txt"}?`, Reply: make(chan bool, 1)}
	m := NewModel("")
	m = apply(t, m, ApprovalMsg{Request: req})
	view := m.View().Content
	if !strings.Contains(view, "Approval") || !strings.Contains(view, "write") {
		t.Fatalf("dialog not rendered: %q", view)
	}
	if !strings.Contains(view, "[y] approve") || !strings.Contains(view, "[n] deny") {
		t.Fatalf("dialog missing y/n hints: %q", view)
	}
	// Typing while the dialog is up must not reach the input line.
	m = apply(t, m, tea.KeyPressMsg{Text: "z", Code: 'z'})
	if strings.Contains(m.View().Content, "> z") {
		t.Fatal("input should be ignored while approval is pending")
	}
}

func TestModelApprovalYesNo(t *testing.T) {
	// Yes.
	req := &ApprovalRequest{Prompt: "Run bash?", Reply: make(chan bool, 1)}
	m := NewModel("")
	m = apply(t, m, ApprovalMsg{Request: req})
	m = apply(t, m, tea.KeyPressMsg{Text: "y", Code: 'y'})
	select {
	case got := <-req.Reply:
		if !got {
			t.Fatal("y should approve")
		}
	default:
		t.Fatal("y did not reply")
	}
	if m.approval != nil {
		t.Fatal("approval should clear after answering")
	}
	// No.
	req2 := &ApprovalRequest{Prompt: "Run bash?", Reply: make(chan bool, 1)}
	m = apply(t, m, ApprovalMsg{Request: req2})
	m = apply(t, m, tea.KeyPressMsg{Text: "n", Code: 'n'})
	select {
	case got := <-req2.Reply:
		if got {
			t.Fatal("n should deny")
		}
	default:
		t.Fatal("n did not reply")
	}
	// Esc also denies.
	req3 := &ApprovalRequest{Prompt: "Run bash?", Reply: make(chan bool, 1)}
	m = apply(t, m, ApprovalMsg{Request: req3})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case got := <-req3.Reply:
		if got {
			t.Fatal("Esc should deny")
		}
	default:
		t.Fatal("Esc did not reply")
	}
}

func TestModelCtrlCQuits(t *testing.T) {
	m := NewModel("")
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("Ctrl-C should return a quit command")
	}
	// The command must be tea.Quit: executing it yields a QuitMsg.
	if msg := cmd(); msg == nil {
		t.Fatal("quit command returned nil message")
	}
}
