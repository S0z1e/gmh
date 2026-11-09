// Package tui is the Bubble Tea v2 interface. It is the only package that
// imports Bubble Tea: the loop, the plain CLI and -p never import it.
// Inline-first: finished turns are committed to scrollback with
// tea.Println, only the live region (streaming text + current tool +
// input line) is repainted.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"minimalharness/internal/agent"
)

// Styles are restrained and functional: meta information recedes,
// actions and warnings stand out. No gradients, no decoration for
// its own sake — the TUI is a work surface, not a poster.
var (
	styleTitle   = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	stylePrompt  = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	styleTool    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleUsage   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	styleNotice  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleApprove = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	styleDeny    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
)

// EventMsg wraps one agent event as a Bubble Tea message.
type EventMsg struct {
	Event agent.Event
}

// PrintMsg is one block the runner wants in the scrollback (usage
// lines, /usage output, notices). It goes through the program's message
// queue like every other output, so it can never overtake the model's
// own tea.Println of the finished turn's text: the runner's prog.Println
// used to race that print and the usage lines landed above the reply.
//
// One rule for callers: a PrintMsg may contain embedded newlines, but
// never send two PrintMsgs back-to-back expecting order — consecutive
// tea.Println calls reverse their order in Bubble Tea v2 (verified
// live: turn/session usage printed session-first). Combine the lines
// into one PrintMsg.
type PrintMsg struct {
	Text string
}

// Model is the TUI state.
type Model struct {
	info   string // header block, printed once at startup (see Init)
	input  []rune
	cursor int
	live   string // streamed text of the current turn (a string, not a
	// strings.Builder: Update copies the model by value, and a
	// copied non-zero Builder panics on the next WriteString)
	liveTool string // tool currently running, "" when idle
	working  bool
	width    int
	// approval is non-nil while the agent waits for a y/n answer.
	approval *ApprovalRequest
	// pendingReply is a finished turn's text, waiting to be committed
	// to the scrollback together with the runner's next print (usually
	// the usage lines). It exists because two separate tea.Println
	// commands can execute out of order — each command runs in its
	// own goroutine (Bubble Tea v2.1.1 handleCommands) — so the reply
	// and the usage lines must travel as ONE print, or the usage can
	// land above the reply. Verified live: the ordering test failed
	// exactly this way before this field existed.
	pendingReply string

	// OnSubmit is called with the submitted line on Enter. The caller
	// runs the task and feeds agent events back via program.Send.
	OnSubmit func(string)
}

func NewModel(info string) Model {
	return Model{info: info}
}

// Init prints the header once, into the scrollback, and never again:
// the header is session meta, not part of the live region, so it must
// not sit at the bottom after every turn. (Printing it here is safe —
// Init commands run inside the event loop, unlike prog.Println
// before prog.Run(), which deadlocks.)
func (m Model) Init() tea.Cmd {
	if m.info == "" {
		return nil
	}
	return tea.Println(m.header())
}

// header styles the info block: the first line (title) bold, the rest
// dim — the session meta should recede behind the work.
func (m Model) header() string {
	lines := strings.Split(strings.TrimRight(m.info, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render(lines[0]))
	for _, l := range lines[1:] {
		b.WriteString("\n" + styleDim.Render(l))
	}
	return b.String()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg.Key())
	case tea.KeyReleaseMsg:
		return m, nil // ignore releases; presses already handled
	case EventMsg:
		return m.handleEvent(msg.Event)
	case PrintMsg:
		// The finished turn's reply, if any, goes out in the same
		// print as the runner's text — one Println, so the two can
		// never swap order (see pendingReply). The reply stays
		// plain; the runner's part keeps its own styling.
		if m.pendingReply != "" {
			text := m.pendingReply + "\n" + stylePrint(msg.Text)
			m.pendingReply = ""
			return m, tea.Println(text)
		}
		return m, tea.Println(stylePrint(msg.Text))
	case ApprovalMsg:
		m.approval = msg.Request
		return m, nil
	}
	return m, nil
}

func (m Model) handleKey(k tea.Key) (tea.Model, tea.Cmd) {
	// Ctrl-C quits.
	if k.Mod == tea.ModCtrl && k.Code == 'c' {
		return m, tea.Quit
	}
	// While an approval is pending, the dialog replaces the input line:
	// only y/n (or Esc for no) are accepted, no Enter needed.
	if m.approval != nil {
		switch {
		case k.Code == 'y' || k.Code == 'Y':
			m.approval.Reply <- true
			m.approval = nil
		case k.Code == 'n' || k.Code == 'N' || k.Code == tea.KeyEscape:
			m.approval.Reply <- false
			m.approval = nil
		}
		return m, nil
	}
	switch k.Code {
	case tea.KeyEnter:
		line := string(m.input)
		m.input = nil
		m.cursor = 0
		if m.OnSubmit != nil {
			m.OnSubmit(line)
		}
		return m, nil
	case tea.KeyBackspace:
		if m.cursor > 0 {
			m.input = append(m.input[:m.cursor-1], m.input[m.cursor:]...)
			m.cursor--
		}
		return m, nil
	case tea.KeyLeft:
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case tea.KeyRight:
		if m.cursor < len(m.input) {
			m.cursor++
		}
		return m, nil
	}
	// Printable characters (k.Text is non-empty for them).
	if k.Text != "" {
		r := []rune(k.Text)
		m.input = append(m.input[:m.cursor], append(r, m.input[m.cursor:]...)...)
		m.cursor += len(r)
	}
	return m, nil
}

func (m Model) handleEvent(ev agent.Event) (tea.Model, tea.Cmd) {
	switch ev.Kind {
	case agent.EventTextDelta:
		// A new turn starting with a reply still pending (no runner
		// print followed the last turn): commit the reply on its own
		// first, so it is never lost behind the new turn.
		if m.pendingReply != "" {
			reply := m.pendingReply
			m.pendingReply = ""
			m.live += ev.Text
			m.working = true
			return m, tea.Println(reply)
		}
		m.live += ev.Text
		m.working = true
	case agent.EventToolStart:
		m.liveTool = ev.ToolName
		m.working = true
	case agent.EventToolEnd:
		m.liveTool = ""
	case agent.EventStepUsage:
		// Usage lines are printed by the runner via PrintMsg, through
		// the same queue, so they land after the reply text.
	case agent.EventToolBackgrounded:
		// The tool keeps running as a job; it no longer blocks the
		// live region. Tell the scrollback, like a finished turn.
		m.liveTool = ""
		return m, tea.Println(styleNotice.Render(fmt.Sprintf("… bash running in background (%s)", ev.JobID)))
	case agent.EventBackgroundWait:
		return m, tea.Println(styleNotice.Render(fmt.Sprintf("waiting for background jobs: %s", ev.Text)))
	case agent.EventToolBackgroundDone:
		if ev.ToolErr != nil {
			return m, tea.Println(styleError.Render(fmt.Sprintf("background job %s finished with error: %v", ev.JobID, ev.ToolErr)))
		}
		return m, tea.Println(styleNotice.Render(fmt.Sprintf("background job %s finished", ev.JobID)))
	case agent.EventDone:
		// Do not print the reply here: hold it as pendingReply and
		// let the runner's next print (the usage lines) carry it out
		// in the same Println. Two separate Println commands can
		// swap order; one print cannot.
		m.pendingReply = strings.TrimRight(m.live, "\n")
		m.live = ""
		m.liveTool = ""
		m.working = false
	case agent.EventError:
		m.liveTool = ""
		m.working = false
		// The error line itself is printed by the runner.
	}
	return m, nil
}

// stylePrint colours runner output for the scrollback: usage and
// compaction notes recede, errors stand out, everything else is plain.
// The runner's prints often start with a blank line ("\n[usage] …",
// "\n[error] …"), so the prefix check runs on the text with leading
// newlines stripped — otherwise nothing here would ever match.
func stylePrint(s string) string {
	trimmed := strings.TrimLeft(s, "\n")
	switch {
	case strings.HasPrefix(trimmed, "[usage]"), strings.HasPrefix(trimmed, "[compact]"):
		return styleUsage.Render(s)
	case strings.HasPrefix(trimmed, "[error]"):
		return styleError.Render(s)
	default:
		return s
	}
}

// View renders only the live region: the streaming text with the
// current tool, and the input line (or the approval dialog, which
// replaces the input line while it is pending). The header was
// printed once at startup (see Init); finished turns are already in
// scrollback via tea.Println.
func (m Model) View() tea.View {
	var b strings.Builder
	if m.working || m.live != "" {
		b.WriteString(m.live)
		if m.liveTool != "" {
			b.WriteString("\n" + styleTool.Render("▶ "+m.liveTool+"…"))
		}
		b.WriteString("\n")
	}
	if m.approval != nil {
		b.WriteString(m.approvalBox())
	} else {
		b.WriteString(stylePrompt.Render("❯ ") + string(m.input))
	}
	return tea.NewView(b.String())
}

// approvalBox renders the pending approval as a dialog: an amber
// rounded border, a bold title, the prompt, and y in green / n in
// red. It stays inline-first (no alt-screen overlay); lipgloss only
// styles the text, it does not take over the screen.
func (m Model) approvalBox() string {
	width := 60
	if m.width > 8 && m.width-4 < width {
		width = m.width - 4
	}
	if width < 20 {
		width = 20
	}
	// Inner width: the border (2) and padding (2) come off the total.
	inner := width - 4
	if inner < 10 {
		inner = 10
	}
	lines := wrapText(m.approval.Prompt, inner)
	body := styleTitle.Render("Approval") + "\n" + strings.Join(lines, "\n") + "\n" +
		styleApprove.Render("[y] approve") + "   " + styleDeny.Render("[n] deny")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(0, 1).
		Width(inner).
		Render(body)
}

// wrapText word-wraps s at width runes.
func wrapText(s string, width int) []string {
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		cur := words[0]
		for _, w := range words[1:] {
			if len([]rune(cur))+1+len([]rune(w)) > width {
				lines = append(lines, cur)
				cur = w
			} else {
				cur += " " + w
			}
		}
		lines = append(lines, cur)
	}
	return lines
}
