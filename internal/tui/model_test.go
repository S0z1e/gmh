package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"minimalharness/internal/agent"
)

// stripANSI removes SGR escape sequences so assertions can match the
// visible text of a styled view (the styled input "┃ hi" is split by
// a reset code, so the literal "┃ hi" never appears in raw output).
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
		prog.Send(PrintMsg{Text: "[usage] turn: prompt=1 (cached=0) completion=0 total=1"})
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
		// The usage line is restyled in the TUI (brackets off, "·"
		// separators), so search for the restyled text, not "[usage]".
		iReply := strings.LastIndex(s, "hello reply")
		iUsage := strings.Index(s, "turn · prompt 1")
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

// Usage lines are restyled in the TUI (brackets off, "·" separators,
// thousands separators) and errors become "✗ <text>"; the literal
// numbers must survive the styling.
func TestStylePrintRestylesUsageAndErrors(t *testing.T) {
	if got := stripANSI(stylePrint("[usage] turn: prompt=3922 (cached=1920) completion=215 total=4137")); got != "   turn · prompt 3,922 (cached 1,920) · completion 215 · total 4,137" {
		t.Fatalf("usage restyled wrong: %q", got)
	}
	if got := stripANSI(stylePrint("[usage] session: prompt=8457 (cached=0) completion=573 total=9030 | fee: Discovery 實價未驗證")); got != "   session · prompt 8,457 (cached 0) · completion 573 · total 9,030 · fee: Discovery 實價未驗證" {
		t.Fatalf("session usage restyled wrong: %q", got)
	}
	if got := stripANSI(stylePrint("[error] step 1: boom")); got != "   ✗ step 1: boom" {
		t.Fatalf("error restyled wrong: %q", got)
	}
	// The runner's prints start with a blank line: "\n[usage] …" and
	// "\n[error] …" must still match, and actually get styled.
	if got := stylePrint("\n[usage] turn: prompt=1 (cached=0) completion=0 total=1"); got == "\n[usage] turn: prompt=1 (cached=0) completion=0 total=1" {
		t.Fatalf("usage with leading newline was not styled: %q", got)
	}
	if got := stylePrint("\n[error] boom"); got == "\n[error] boom" {
		t.Fatalf("error with leading newline was not styled: %q", got)
	}
	if got := stylePrint("plain notice"); got != "plain notice" {
		t.Fatalf("plain text must pass through unstyled, got %q", got)
	}
	// An unrecognised usage shape is not eaten: the prefix is
	// stripped, the rest kept.
	if got := stripANSI(stylePrint("[usage] something odd")); got != "   something odd" {
		t.Fatalf("odd usage restyled wrong: %q", got)
	}
}

func TestFormatInt(t *testing.T) {
	for in, want := range map[int]string{0: "0", 1: "1", 999: "999", 1000: "1,000", 3922: "3,922", 1000000: "1,000,000"} {
		if got := formatInt(in); got != want {
			t.Errorf("formatInt(%d) = %q, want %q", in, got, want)
		}
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
	// The running row shows the tool's pending label, naming the object.
	if got := m.View().Content; !strings.Contains(got, `Searching "TODO"`) {
		t.Fatalf("live region should show the running tool, got %q", got)
	}
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventToolEnd, ToolName: "grep"}})
	if got := m.View().Content; strings.Contains(got, `Searching "TODO"`) {
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
	if got := stripANSI(m.View().Content); !strings.Contains(got, "┃ hi") {
		t.Fatalf("input line = %q, want '┃ hi'", got)
	}
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter should return the user-panel echo command")
	}
	m = next.(Model)
	if len(submitted) != 1 || submitted[0] != "hi" {
		t.Fatalf("submitted = %v, want [hi]", submitted)
	}
	if got := stripANSI(m.View().Content); strings.Contains(got, "┃ hi") {
		t.Fatalf("input should clear after Enter, got %q", got)
	}
	// A submitted task starts the turn; the Thinking row waits out
	// the spinner delay, so it is absent at first and appears later.
	if !m.working {
		t.Fatal("model should be working after submitting a task")
	}
	if got := stripANSI(m.View().Content); strings.Contains(got, "Thinking") {
		t.Fatalf("live region should not show Thinking yet, got %q", got)
	}
	m.workingSince = m.workingSince.Add(-2 * spinnerDelay)
	if got := stripANSI(m.View().Content); !strings.Contains(got, "Thinking") {
		t.Fatalf("live region should show Thinking after the delay, got %q", got)
	}
	// A bare Enter echoes nothing and starts no turn.
	m2 := NewModel("")
	m2.OnSubmit = func(s string) {}
	_, cmd2 := m2.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd2 != nil {
		t.Fatal("bare Enter should return no command")
	}
	if m2.working {
		t.Fatal("bare Enter should not start a turn")
	}
}

func TestModelBackspaceAndCursor(t *testing.T) {
	m := NewModel("")
	for _, r := range "ab" {
		m = apply(t, m, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := stripANSI(m.View().Content); !strings.Contains(got, "┃ a") || strings.Contains(got, "┃ ab") {
		t.Fatalf("after backspace input = %q, want '┃ a'", got)
	}
}

func TestModelApprovalDialogReplacesInput(t *testing.T) {
	req := &ApprovalRequest{Prompt: `Run write {"path":"x.txt"}?`, Reply: make(chan bool, 1)}
	m := NewModel("")
	m = apply(t, m, ApprovalMsg{Request: req})
	view := m.View().Content
	if !strings.Contains(view, "Approval required") || !strings.Contains(view, "Write x.txt") {
		t.Fatalf("dialog not rendered: %q", view)
	}
	if !strings.Contains(view, "[y] Allow once") || !strings.Contains(view, "[n] Deny") {
		t.Fatalf("dialog missing y/n hints: %q", view)
	}
	if !strings.Contains(view, "esc = deny") {
		t.Fatalf("dialog missing esc hint: %q", view)
	}
	// A left-border panel (┃), not the old rounded box (╭/╰).
	if !strings.Contains(view, "┃") {
		t.Fatalf("dialog should have a left border panel: %q", view)
	}
	if strings.Contains(view, "╭") || strings.Contains(view, "╰") {
		t.Fatalf("dialog must not have a rounded border: %q", view)
	}
	// Typing while the dialog is up must not reach the input line.
	m = apply(t, m, tea.KeyPressMsg{Text: "z", Code: 'z'})
	if strings.Contains(stripANSI(m.View().Content), "┃ z") {
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

func TestHeaderLine(t *testing.T) {
	got := stripANSI(headerLine("▪ harness · chat · deepseek-v4.1-flash"))
	if got != "▪ harness · chat · deepseek-v4.1-flash" {
		t.Fatalf("headerLine = %q", got)
	}
	got = stripANSI(headerLine("harness · chat · model"))
	if got != "harness · chat · model" {
		t.Fatalf("headerLine (old form) = %q", got)
	}
}

func TestMetaLine(t *testing.T) {
	m := NewModel("")
	if got := m.metaLine(); got != "" {
		t.Fatalf("meta line without meta = %q, want empty", got)
	}
	m = m.WithMeta("suggest", "chat", "deepseek-v4.1-flash")
	got := stripANSI(m.metaLine())
	if !strings.Contains(got, "suggest") || !strings.Contains(got, "deepseek-v4.1-flash") || !strings.Contains(got, "chat") {
		t.Fatalf("meta line = %q", got)
	}
	if got := stripANSI(m.View().Content); !strings.Contains(got, "suggest") {
		t.Fatalf("view should show the meta line, got %q", got)
	}
}

func TestPlaceholderShownWhenInputEmpty(t *testing.T) {
	m := NewModel("")
	got := stripANSI(m.View().Content)
	if !strings.Contains(got, "Ask anything") || !strings.Contains(got, "/usage") {
		t.Fatalf("empty input should show the placeholder, got %q", got)
	}
}

func TestToolSummary(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"read", `{"path":"t.txt"}`, "Read t.txt"},
		{"write", `{"path":"u.txt","content":"x"}`, "Write u.txt"},
		{"edit", `{"path":"a.go"}`, "Edit a.go"},
		{"grep", `{"pattern":"TODO","path":"."}`, `Grep "TODO" in .`},
		{"glob", `{"pattern":"*.go"}`, `Glob "*.go"`},
		{"bash", `{"command":"ls -la"}`, "$ ls -la"},
		{"load_skill", `{"name":"review"}`, "Load skill review"},
		{"read", `not json`, "read"},
		{"read", `{}`, "read"},
		{"unknown", `{"path":"x"}`, "unknown"},
	}
	for _, tc := range cases {
		if got := toolSummary(tc.name, tc.args); got != tc.want {
			t.Errorf("toolSummary(%q, %q) = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestToolPendingLabels(t *testing.T) {
	for _, name := range []string{"read", "write", "edit", "grep", "glob", "bash", "load_skill"} {
		if got := toolPendingLabel(name, ""); got == "" || got == name {
			t.Errorf("toolPendingLabel(%q, \"\") = %q", name, got)
		}
	}
	if got := toolPendingLabel("weird", ""); got != "weird…" {
		t.Errorf("toolPendingLabel(weird) = %q, want %q", got, "weird…")
	}
	// With args the label names the concrete object.
	cases := []struct {
		name, args, want string
	}{
		{"read", `{"path":"src/foo.ts"}`, "Reading src/foo.ts…"},
		{"write", `{"path":"u.txt","content":"x"}`, "Preparing write u.txt…"},
		{"edit", `{"path":"a.go"}`, "Preparing edit a.go…"},
		{"grep", `{"pattern":"TODO","path":"."}`, `Searching "TODO" in .…`},
		{"grep", `{"pattern":"TODO"}`, `Searching "TODO"…`},
		{"glob", `{"pattern":"*.go"}`, `Finding "*.go"…`},
		{"bash", `{"command":"ls -la"}`, "Running ls -la…"},
		{"load_skill", `{"name":"review"}`, "Loading skill review…"},
		{"read", `not json`, "Reading file…"},
		{"read", `{}`, "Reading file…"},
	}
	for _, tc := range cases {
		if got := toolPendingLabel(tc.name, tc.args); got != tc.want {
			t.Errorf("toolPendingLabel(%q, %q) = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestCollapseOutput(t *testing.T) {
	lines, cut := collapseOutput("a\nb\nc")
	if len(lines) != 3 || cut != 0 {
		t.Fatalf("short output: %v cut %d", lines, cut)
	}
	var b strings.Builder
	for i := 1; i <= 12; i++ {
		b.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	lines, cut = collapseOutput(b.String())
	if len(lines) != 10 || cut != 2 {
		t.Fatalf("12 lines: got %d lines cut %d", len(lines), cut)
	}
	if lines[0] != "line 3" || lines[9] != "line 12" {
		t.Fatalf("tail wrong: %v", lines)
	}
	if lines, cut := collapseOutput(""); len(lines) != 0 || cut != 0 {
		t.Fatalf("empty: %v cut %d", lines, cut)
	}
	// One huge line is capped at 200 runes.
	lines, _ = collapseOutput(strings.Repeat("x", 500))
	if len([]rune(lines[0])) > 200 {
		t.Fatalf("long line not capped: %d runes", len([]rune(lines[0])))
	}
}

func TestToolEndBlock(t *testing.T) {
	// Success: green tick, muted summary, 3-space gutter.
	got := stripANSI(toolEndBlock("read", `{"path":"t.txt"}`, "1\tomega\n", nil))
	if got != "   ✓ Read t.txt" {
		t.Fatalf("success row = %q", got)
	}
	// Failure: red cross plus the error text below.
	got = stripANSI(toolEndBlock("read", `{"path":"t.txt"}`, "Error: file not found", errTest))
	if !strings.Contains(got, "✗ Read t.txt") || !strings.Contains(got, "Error: file not found") {
		t.Fatalf("failure row = %q", got)
	}
	// Denied: muted cross with "(denied)", never a green tick.
	got = stripANSI(toolEndBlock("write", `{"path":"x.txt"}`, "User denied this tool call. Do not retry it.", nil))
	if got != "   ✗ Write x.txt (denied)" {
		t.Fatalf("denied row = %q", got)
	}
	// Bash: a block with the command in the top border and the output tail.
	got = stripANSI(toolEndBlock("bash", `{"command":"echo hi"}`, "hi\n", nil))
	if !strings.Contains(got, "┤ $ echo hi ├") || !strings.Contains(got, "hi") {
		t.Fatalf("bash block = %q", got)
	}
	var b strings.Builder
	for i := 1; i <= 12; i++ {
		b.WriteString("out " + strconv.Itoa(i) + "\n")
	}
	got = stripANSI(toolEndBlock("bash", `{"command":"seq 12"}`, b.String(), nil))
	if !strings.Contains(got, "(2 earlier lines)") || !strings.Contains(got, "out 12") || strings.Contains(got, "out 1\n") {
		t.Fatalf("bash block collapse = %q", got)
	}
}

var errTest = errTestType("boom")

type errTestType string

func (e errTestType) Error() string { return string(e) }

func TestParseApprovalPrompt(t *testing.T) {
	tool, summary, detail := parseApprovalPrompt(`Run write {"path":"dialog.txt","content":"hi"}?`)
	if tool != "write" || summary != "Write dialog.txt" || detail != `{"path":"dialog.txt","content":"hi"}` {
		t.Fatalf("parsed = (%q, %q, %q)", tool, summary, detail)
	}
	tool, summary, detail = parseApprovalPrompt("Run bash?")
	if tool != "" || summary != "Run bash?" {
		t.Fatalf("unparseable prompt = (%q, %q, %q)", tool, summary, detail)
	}
}

func TestSpinnerTickCycle(t *testing.T) {
	m := NewModel("")
	m.OnSubmit = func(s string) {}
	m = apply(t, m, tea.KeyPressMsg{Text: "h", Code: 'h'})
	m = apply(t, m, tea.KeyPressMsg{Text: "i", Code: 'i'})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.working {
		t.Fatal("should be working after submitting a task")
	}
	// The spinner advances on each tick while the turn runs with no text,
	// but the Thinking row waits out the delay: a fresh turn shows no
	// spinner at all, so short turns never flash one.
	f0 := m.spinnerFrame
	m = apply(t, m, spinnerTickMsg{})
	if m.spinnerFrame == f0 {
		t.Fatal("spinner frame should advance on a tick")
	}
	if got := stripANSI(m.View().Content); strings.Contains(got, "Thinking") {
		t.Fatalf("fresh turn should not show Thinking yet, got %q", got)
	}
	// Once the delay has passed, the row appears.
	m.workingSince = m.workingSince.Add(-2 * spinnerDelay)
	if got := stripANSI(m.View().Content); !strings.Contains(got, "Thinking") {
		t.Fatalf("view should show Thinking after the delay, got %q", got)
	}
	// The first text delta replaces the spinner with the text.
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventTextDelta, Text: "hello"}})
	if got := stripANSI(m.View().Content); !strings.Contains(got, "hello") || strings.Contains(got, "Thinking") {
		t.Fatalf("view after text delta = %q", got)
	}
	// A tool start shows the running row with its pending label,
	// naming the concrete object.
	m = apply(t, m, EventMsg{Event: agent.Event{Kind: agent.EventToolStart, ToolName: "read", ToolArgs: `{"path":"t.txt"}`}})
	if got := stripANSI(m.View().Content); !strings.Contains(got, "Reading t.txt") {
		t.Fatalf("view during tool = %q", got)
	}
	// Tool end prints the finished row and clears the tool.
	next, cmd := m.Update(EventMsg{Event: agent.Event{Kind: agent.EventToolEnd, ToolName: "read", ToolOutput: "1\tx\n"}})
	if cmd == nil {
		t.Fatal("ToolEnd should return a print command")
	}
	if next.(Model).liveTool != "" {
		t.Fatal("tool should clear after ToolEnd")
	}
}

func TestUserPanel(t *testing.T) {
	got := stripANSI(userPanel("read t.txt then write it", 0))
	if !strings.Contains(got, "┃") || !strings.Contains(got, "read t.txt then write it") {
		t.Fatalf("user panel = %q", got)
	}
}

func TestSpinnerDelay(t *testing.T) {
	m := NewModel("")
	m.working = true
	m.live = ""
	// Just started: no Thinking row, however many ticks fire.
	m.workingSince = time.Now()
	if got := stripANSI(m.View().Content); strings.Contains(got, "Thinking") {
		t.Fatalf("fresh turn should not show Thinking, got %q", got)
	}
	// Past the delay: the row appears.
	m.workingSince = time.Now().Add(-2 * spinnerDelay)
	if got := stripANSI(m.View().Content); !strings.Contains(got, "Thinking") {
		t.Fatalf("view after the delay should show Thinking, got %q", got)
	}
	// Done clears it: working false means no Thinking whatever the clock says.
	m.working = false
	m.workingSince = time.Time{}
	if got := stripANSI(m.View().Content); strings.Contains(got, "Thinking") {
		t.Fatalf("idle view should not show Thinking, got %q", got)
	}
}

func TestMetaLineRight(t *testing.T) {
	m := NewModel("").WithMeta("auto", "chat", "mock").WithBranch("main")
	if got := stripANSI(m.metaLine()); !strings.Contains(got, "main") {
		t.Fatalf("meta line should show the branch, got %q", got)
	}
	m = apply(t, m, MetaUpdateMsg{Tokens: 12345, Turns: 3})
	got := stripANSI(m.metaLine())
	if !strings.Contains(got, "main") || !strings.Contains(got, "3 turns") || !strings.Contains(got, "12,345 tokens") {
		t.Fatalf("meta line with totals = %q", got)
	}
	// MetaUpdateMsg alone does not create a meta line: the mode/model/api
	// still gate it.
	m2 := apply(t, NewModel(""), MetaUpdateMsg{Tokens: 1, Turns: 1})
	if m2.metaLine() != "" {
		t.Fatalf("meta line without meta = %q", m2.metaLine())
	}
}
