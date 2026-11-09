package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"minimalharness/internal/agent"
)

// stripANSI removes SGR escape sequences so assertions can match the
// visible text of a styled view (the styled prompt "❯ " is split by
// a reset code, so the literal "❯ hi" never appears in raw output).
func stripANSI(s string) string {
	return ansi.Strip(s)
}

func apply(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return nm
}

// A PrintMsg sent right after the done event must land below the
// reply text in the scrollback, never above it. The runner used to
// call prog.Println directly, which raced the model's tea.Println of
// the turn text and printed the usage lines first. Run the race
// several times: with the queue it is deterministic.
func TestPrintMsgOrderedAfterDoneText(t *testing.T) {
	for i := 0; i < 5; i++ {
		// A real renderer (not WithoutRenderer): tea.Println output
		// only reaches the output writer through the renderer.
		// syncBuffer, not bytes.Buffer: Bubble Tea writes to the
		// output from two goroutines (see sync_buffer_test.go).
		var out syncBuffer
		prog := tea.NewProgram(NewModel(""), tea.WithInput(strings.NewReader("")), tea.WithOutput(&out))
		done := make(chan struct{})
		go func() {
			_, _ = prog.Run()
			close(done)
		}()
		// Stream the reply, then finish the turn and immediately ask
		// the runner to print a usage line — the two sends race.
		prog.Send(EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "hello reply"}})
		prog.Send(EventMsg{Event: agent.Event{Kind: agent.EventDone, Text: "hello reply"}})
		prog.Send(PrintMsg{Text: "[usage] turn: prompt=1"})
		time.Sleep(300 * time.Millisecond)
		prog.Quit()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("program did not quit")
		}
		s := out.String()
		// The reply also appears in live-region renders while it
		// streams; the last occurrence is the Println commit at Done.
		iReply := strings.LastIndex(s, "hello reply")
		iUsage := strings.Index(s, "[usage] turn:")
		if iReply < 0 || iUsage < 0 || iReply > iUsage {
			t.Fatalf("iteration %d: reply at %d, usage at %d in %q", i, iReply, iUsage, s)
		}
	}
}

// The header is printed once at startup (Init) and is not part of
// the live region: it must not sit in the view after every turn.
func TestModelHeaderPrintsOnceNotInView(t *testing.T) {
	m := NewModel("harness · chat · model\nroot /tmp/x · session s1")
	if m.Init() == nil {
		t.Fatal("Init should return a print command when info is set")
	}
	if got := m.View().Content; strings.Contains(got, "harness") || strings.Contains(got, "root /tmp/x") {
		t.Fatalf("header must not be in the live view, got %q", got)
	}
	if m := NewModel(""); m.Init() != nil {
		t.Fatal("Init should return nil when info is empty")
	}
}

// Usage lines are dimmed, errors stand out — the literal text must
// survive the styling so the ordering test can still find it.
func TestStylePrintKeepsLiteralText(t *testing.T) {
	if got := stylePrint("[usage] turn: prompt=1"); !strings.Contains(got, "[usage] turn: prompt=1") {
		t.Fatalf("usage text lost in styling: %q", got)
	}
	if got := stylePrint("[error] step 1: boom"); !strings.Contains(got, "[error] step 1: boom") {
		t.Fatalf("error text lost in styling: %q", got)
	}
	// The runner's prints start with a blank line: "\n[usage] …" and
	// "\n[error] …" must still match, and actually get styled.
	if got := stylePrint("\n[usage] turn: prompt=1"); got == "\n[usage] turn: prompt=1" {
		t.Fatalf("usage with leading newline was not styled: %q", got)
	}
	if got := stylePrint("\n[error] boom"); got == "\n[error] boom" {
		t.Fatalf("error with leading newline was not styled: %q", got)
	}
	if got := stylePrint("plain notice"); got != "plain notice" {
		t.Fatalf("plain text must pass through unstyled, got %q", got)
	}
}

// Streaming must survive many text deltas through a real program:
// the live text used to be a strings.Builder, which panics ("illegal
// use of non-zero Builder copied by value") on the second write after
// Bubble Tea copies the model — a real multi-chunk reply killed the
// program mid-turn. It is a plain string now.
func TestModelStreamsManyDeltas(t *testing.T) {
	var out syncBuffer
	prog := tea.NewProgram(NewModel(""), tea.WithInput(strings.NewReader("")), tea.WithOutput(&out))
	done := make(chan struct{})
	go func() {
		_, _ = prog.Run()
		close(done)
	}()
	for _, d := range []string{"one ", "two ", "three ", "four ", "five"} {
		prog.Send(EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: d}})
	}
	prog.Send(EventMsg{Event: agent.Event{Kind: agent.EventDone, Text: "one two three four five"}})
	prog.Send(PrintMsg{Text: "[usage] turn: prompt=1"})
	time.Sleep(300 * time.Millisecond)
	prog.Quit()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("program did not quit")
	}
	s := out.String()
	if !strings.Contains(s, "one two three four five") {
		t.Fatalf("streamed text missing from output: %q", s)
	}
}

// PrintMsg renders through tea.Println.
func TestPrintMsgReturnsPrintln(t *testing.T) {
	m := NewModel("")
	_, cmd := m.Update(PrintMsg{Text: "hi"})
	if cmd == nil {
		t.Fatal("PrintMsg should return a command")
	}
}

// Background events clear the live tool and print a scrollback notice.
func TestModelBackgroundEvents(t *testing.T) {
	m := NewModel("")
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "working"}})
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventToolStart, ToolName: "bash"}})
	if m.liveTool != "bash" {
		t.Fatalf("liveTool = %q, want bash", m.liveTool)
	}
	next, cmd := m.Update(EventMsg{Event: agent.Event{Kind: agent.EventToolBackgrounded, JobID: "bg-1"}})
	if cmd == nil {
		t.Fatal("backgrounded event should return a print command")
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	if nm.liveTool != "" {
		t.Fatalf("liveTool = %q after backgrounded, want empty", nm.liveTool)
	}
	if _, cmd := nm.Update(EventMsg{Event: agent.Event{Kind: agent.EventBackgroundWait, Text: "bg-1"}}); cmd == nil {
		t.Fatal("background wait event should return a print command")
	}
	if _, cmd := nm.Update(EventMsg{Event: agent.Event{Kind: agent.EventToolBackgroundDone, JobID: "bg-1"}}); cmd == nil {
		t.Fatal("background done event should return a print command")
	}
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
	// Done holds the reply as pendingReply instead of printing it:
	// the runner's next print carries it out in the same Println,
	// so the reply and the usage lines can never swap order.
	if cmd != nil {
		t.Fatal("Done should not print on its own; the reply waits for the next print")
	}
	if m.pendingReply != "done text" {
		t.Fatalf("pendingReply = %q, want %q", m.pendingReply, "done text")
	}
	if m.working {
		t.Fatal("model should be idle after Done")
	}
	if got := m.View().Content; strings.Contains(got, "done text") {
		t.Fatalf("live region should be cleared after Done, got %q", got)
	}
	// The next print flushes the reply together with its own text.
	next, cmd = m.Update(PrintMsg{Text: "[usage] turn: prompt=1"})
	if cmd == nil {
		t.Fatal("PrintMsg should return a print command")
	}
	if next.(Model).pendingReply != "" {
		t.Fatal("pendingReply should clear after the print")
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
	if got := stripANSI(m.View().Content); !strings.Contains(got, "❯ hi") {
		t.Fatalf("input line = %q, want '❯ hi'", got)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(submitted) != 1 || submitted[0] != "hi" {
		t.Fatalf("submitted = %v, want [hi]", submitted)
	}
	if got := stripANSI(m.View().Content); strings.Contains(got, "❯ hi") {
		t.Fatalf("input should clear after Enter, got %q", got)
	}
}

func TestModelBackspaceAndCursor(t *testing.T) {
	m := NewModel("")
	for _, r := range "ab" {
		m = apply(t, m, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := stripANSI(m.View().Content); !strings.Contains(got, "❯ a") || strings.Contains(got, "❯ ab") {
		t.Fatalf("after backspace input = %q, want '❯ a'", got)
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
	// Rounded lipgloss border, not the old square box-drawing.
	if !strings.Contains(view, "╭") || !strings.Contains(view, "╰") {
		t.Fatalf("dialog should have a rounded border: %q", view)
	}
	// Typing while the dialog is up must not reach the input line.
	m = apply(t, m, tea.KeyPressMsg{Text: "z", Code: 'z'})
	if strings.Contains(stripANSI(m.View().Content), "❯ z") {
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
