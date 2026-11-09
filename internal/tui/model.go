// Package tui is the Bubble Tea v2 interface. It is the only package that
// imports Bubble Tea: the loop, the plain CLI and -p never import it.
// Inline-first: finished turns are committed to scrollback with
// tea.Println, only the live region (streaming text + current tool +
// input line) is repainted.
package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"minimalharness/internal/agent"
)

// EventMsg wraps one agent event as a Bubble Tea message.
type EventMsg struct {
	Event agent.Event
}

// Model is the TUI state.
type Model struct {
	info     string // e.g. resumed-session message, shown once at the top
	input    []rune
	cursor   int
	live     strings.Builder // streamed text of the current turn
	liveTool string          // tool currently running, "" when idle
	working  bool
	width    int

	// OnSubmit is called with the submitted line on Enter. The caller
	// runs the task and feeds agent events back via program.Send.
	OnSubmit func(string)
}

func NewModel(info string) Model {
	return Model{info: info}
}

func (m Model) Init() tea.Cmd {
	return nil
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
	}
	return m, nil
}

func (m Model) handleKey(k tea.Key) (tea.Model, tea.Cmd) {
	// Ctrl-C quits.
	if k.Mod == tea.ModCtrl && k.Code == 'c' {
		return m, tea.Quit
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
		m.live.WriteString(ev.Text)
		m.working = true
	case agent.EventToolStart:
		m.liveTool = ev.ToolName
		m.working = true
	case agent.EventToolEnd:
		m.liveTool = ""
	case agent.EventStepUsage:
		// Usage lines are printed by the runner via program.Println.
	case agent.EventDone:
		text := strings.TrimRight(m.live.String(), "\n")
		m.live.Reset()
		m.liveTool = ""
		m.working = false
		if text != "" {
			return m, tea.Println(text)
		}
	case agent.EventError:
		m.liveTool = ""
		m.working = false
		// The error line itself is printed by the runner.
	}
	return m, nil
}

// View renders only the live region: the info line, the streaming text
// with the current tool, and the input line. Finished turns are already
// in scrollback via tea.Println.
func (m Model) View() tea.View {
	var b strings.Builder
	if m.info != "" {
		b.WriteString(m.info + "\n")
	}
	if m.working || m.live.Len() > 0 {
		b.WriteString(m.live.String())
		if m.liveTool != "" {
			b.WriteString("\n▶ " + m.liveTool + "…")
		}
		b.WriteString("\n")
	}
	b.WriteString("> " + string(m.input))
	return tea.NewView(b.String())
}
