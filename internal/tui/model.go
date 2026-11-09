// Package tui is the Bubble Tea v2 interface. It is the only package that
// imports Bubble Tea: the loop, the plain CLI and -p never import it.
// Inline-first: finished turns are committed to scrollback with
// tea.Println, only the live region (streaming text + current tool +
// input line) is repainted.
//
// Look: opencode's visual language (restyled 2026-11-09 from the
// research in ~/workspace/research_notes/opencode-tui-look-20261109/):
// near-black palette, one peach accent, user turns as raised panels
// with a peach left edge, assistant text indented 3 spaces with a
// small markdown subset, tools as muted icon rows, bash output as a
// collapsed raised block, and the input as a raised box with a heavy
// left border and a model line. opencode itself ships this inline form
// (opencode mini), so the look needs no alt-screen.
package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"minimalharness/internal/agent"
)

// Palette and styles live in theme.go: one Theme struct, rebuilt by
// SetTheme. Everything below renders through those package-level
// styles, so a theme switch is one call at startup.

// spinnerDelay is how long a turn must run with no text before the
// Thinking row appears (Claude Code waits 2s; our turns are shorter,
// so 300ms). Short turns never flash a spinner at all — that flash
// was the turn-end flicker: a fast turn showed Thinking for one frame
// and it was gone by the next repaint.
const spinnerDelay = 300 * time.Millisecond

// spinnerFrames is opencode's braille spinner (80 ms per frame).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerGlyph(frame int) string {
	return spinnerFrames[frame%len(spinnerFrames)]
}

type spinnerTickMsg struct{}

func spinnerTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

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
	liveTool     string // tool currently running, "" when idle
	liveToolArgs string // JSON args of the running tool (ToolEnd
	// carries no args, so the summary is built from these)
	working bool
	// workingSince is when the current turn started working; the
	// Thinking row only shows once spinnerDelay has passed.
	workingSince time.Time
	width        int
	spinnerFrame int
	spinning     bool // a spinner tick loop is running
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

	// Meta for the composer line under the input (mode · model · api).
	// Set by WithMeta; empty in tests, which then show no meta line.
	metaMode  string
	metaModel string
	metaAPI   string
	// metaBranch is the workspace's git branch ("" when the root is
	// not a git checkout), set by WithBranch.
	metaBranch string
	// sessionTokens/turnCount accumulate across turns, fed by
	// MetaUpdateMsg from the runner; shown on the meta line's right.
	sessionTokens int
	turnCount     int

	// OnSubmit is called with the submitted line on Enter. The caller
	// runs the task and feeds agent events back via program.Send.
	OnSubmit func(string)
}

func NewModel(info string) Model {
	return Model{info: info}
}

// WithMeta records the mode, model and API for the composer meta line.
// Call it before NewProgram: the program holds its own copy of the
// model, so fields set afterwards on this copy would never show.
func (m Model) WithMeta(mode, api, model string) Model {
	m.metaMode = mode
	m.metaAPI = api
	m.metaModel = model
	return m
}

// WithBranch records the workspace's git branch for the meta line.
// Call it before NewProgram, like WithMeta.
func (m Model) WithBranch(branch string) Model {
	m.metaBranch = branch
	return m
}

// MetaUpdateMsg carries the session totals after a turn, so the meta
// line can show tokens and turns without the runner knowing the TUI.
type MetaUpdateMsg struct {
	Tokens int
	Turns  int
}

// markWorking starts a turn: working becomes true, and workingSince
// records when, so the Thinking row can wait out spinnerDelay. A
// turn that is already working keeps its original start time.
func (m *Model) markWorking() {
	if !m.working {
		m.workingSince = time.Now()
	}
	m.working = true
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

// header styles the info block: the title line gets the opencode-mini
// treatment (muted mark and separators, bold "harness", plain model
// name); the rest is muted session meta.
func (m Model) header() string {
	lines := strings.Split(strings.TrimRight(m.info, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(headerLine(lines[0]))
	for _, l := range lines[1:] {
		b.WriteString("\n" + styleMuted.Render(l))
	}
	return b.String()
}

// headerLine styles one title line of the form
// "▪ harness · api · model" (or the older "harness · api · model").
func headerLine(line string) string {
	parts := strings.Split(line, " · ")
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(styleMuted.Render(" · "))
		}
		switch {
		case p == "▪":
			b.WriteString(styleMuted.Render(p))
		case p == "harness":
			b.WriteString(styleTitle.Render(p))
		case i == len(parts)-1:
			b.WriteString(styleBase.Render(p))
		default:
			b.WriteString(styleMuted.Render(p))
		}
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
	case spinnerTickMsg:
		if m.working && m.live == "" {
			m.spinnerFrame++
			return m, spinnerTick()
		}
		m.spinning = false
		return m, nil
	case EventMsg:
		return m.handleEvent(msg.Event)
	case PrintMsg:
		// The finished turn's reply, if any, goes out in the same
		// print as the runner's text — one Println, so the two can
		// never swap order (see pendingReply). The reply is rendered
		// (indent + markdown); the runner's part keeps its own styling.
		if m.pendingReply != "" {
			text := renderText(m.pendingReply) + "\n" + stylePrint(msg.Text)
			m.pendingReply = ""
			return m, tea.Println(text)
		}
		return m, tea.Println(stylePrint(msg.Text))
	case ApprovalMsg:
		m.approval = msg.Request
		return m, nil
	case MetaUpdateMsg:
		m.sessionTokens = msg.Tokens
		m.turnCount = msg.Turns
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
		if strings.TrimSpace(line) == "" {
			return m, nil
		}
		// Echo the submitted turn as a raised user panel (opencode's
		// user-message panel), printed by the model at submit time —
		// before any agent event for this turn exists, so the ordering
		// guarantees are unaffected.
		echo := tea.Println(userPanel(line, m.width))
		// Slash commands never run the agent loop (and /compact runs
		// the LLM without emitting events), so only a real task starts
		// the thinking spinner. Done/Error always clear working.
		if strings.HasPrefix(line, "/") {
			return m, echo
		}
		m.markWorking()
		if m.spinning {
			return m, echo
		}
		m.spinning = true
		return m, tea.Batch(echo, spinnerTick())
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
			m.markWorking()
			return m, tea.Println(renderText(reply))
		}
		m.live += ev.Text
		m.markWorking()
	case agent.EventToolStart:
		m.liveTool = ev.ToolName
		m.liveToolArgs = ev.ToolArgs
		m.markWorking()
	case agent.EventToolEnd:
		name, args := m.liveTool, m.liveToolArgs
		if name == "" {
			name = ev.ToolName
		}
		m.liveTool = ""
		m.liveToolArgs = ""
		// A backgrounded bash also gets a ToolEnd, with the
		// "still running" observation — its notice was already
		// printed by the ToolBackgrounded event, so print nothing
		// here or the job would be announced twice.
		if name == "bash" && strings.Contains(ev.ToolOutput, "still running in the background") {
			return m, nil
		}
		return m, tea.Println(toolEndBlock(name, args, ev.ToolOutput, ev.ToolErr))
	case agent.EventStepUsage:
		// Usage lines are printed by the runner via PrintMsg, through
		// the same queue, so they land after the reply text.
	case agent.EventToolBackgrounded:
		// The tool keeps running as a job; it no longer blocks the
		// live region. Tell the scrollback, like a finished turn.
		m.liveTool = ""
		m.liveToolArgs = ""
		cmd := ""
		if ev.ToolArgs != "" {
			cmd = toolSummary("bash", ev.ToolArgs)
		}
		if strings.HasPrefix(cmd, "$ ") {
			cmd = cmd[2:]
		}
		if cmd != "" {
			return m, tea.Println(styleWarning.Render("… ") + styleBase.Render("bash "+cmd) + "  " + badgeStyle.Render("Background"))
		}
		return m, tea.Println(styleWarning.Render(fmt.Sprintf("… bash running in background (%s)", ev.JobID)))
	case agent.EventBackgroundWait:
		return m, tea.Println(styleWarning.Render(fmt.Sprintf("waiting for background jobs: %s", ev.Text)))
	case agent.EventToolBackgroundDone:
		if ev.ToolErr != nil {
			return m, tea.Println(styleError.Render(fmt.Sprintf("✗ background job %s finished with error: %v", ev.JobID, ev.ToolErr)))
		}
		return m, tea.Println(styleSuccess.Render(fmt.Sprintf("✓ background job %s finished", ev.JobID)))
	case agent.EventDone:
		// Do not print the reply here: hold it as pendingReply and
		// let the runner's next print (the usage lines) carry it out
		// in the same Println. Two separate Println commands can
		// swap order; one print cannot.
		m.pendingReply = strings.TrimRight(m.live, "\n")
		m.live = ""
		m.liveTool = ""
		m.liveToolArgs = ""
		m.working = false
		m.workingSince = time.Time{}
	case agent.EventError:
		m.liveTool = ""
		m.liveToolArgs = ""
		m.working = false
		m.workingSince = time.Time{}
		// The error line itself is printed by the runner.
	}
	// (Re)start the spinner while a turn runs with no text yet and
	// no tick loop is alive — e.g. the next turn after a textless one.
	if m.working && m.live == "" && !m.spinning {
		m.spinning = true
		return m, spinnerTick()
	}
	return m, nil
}

// toolEndBlock renders a finished tool call for the scrollback. Bash
// gets a raised block with the command and the output tail; every
// other tool gets one muted row with a ✓/✗ icon. A denial (the loop
// reports it as an observation starting "User denied") is a muted
// row with a "(denied)" suffix, never a green tick. All of it sits
// under the 3-space gutter, like the assistant text.
func toolEndBlock(name, argsJSON, output string, toolErr error) string {
	summary := toolSummary(name, argsJSON)
	if name == "bash" {
		return indentBlock(bashBlock(summary, output, toolErr != nil))
	}
	if strings.HasPrefix(output, "User denied") {
		return "   " + styleMuted.Render("✗ "+summary+" (denied)")
	}
	if toolErr != nil {
		row := styleError.Render("✗ " + summary)
		// The expanded error below the row, like opencode.
		errLines, _ := collapseOutput(output)
		if len(errLines) > 0 {
			var b strings.Builder
			b.WriteString("   " + row)
			for _, l := range errLines {
				b.WriteString("\n     " + styleError.Render(l))
			}
			return b.String()
		}
		return "   " + row
	}
	return "   " + styleSuccess.Render("✓") + " " + styleMuted.Render(summary)
}

// indentBlock prefixes every line of a rendered block with the
// 3-space gutter.
func indentBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "   " + l
	}
	return strings.Join(lines, "\n")
}

// bashBlock renders a finished bash call as a raised block: the
// command sits in the top border (btop's ┤title├), then the output
// tail (at most 10 lines) with an "(N earlier lines)" label when
// truncated. On error the title goes error red and the output stays
// muted.
func bashBlock(summary, output string, failed bool) string {
	lines, cut := collapseOutput(output)
	// Title in the border: ┤ $ <command> ├, padded with ─ to the
	// widest output line so the block reads as one framed box. The
	// command is capped so one long line cannot stretch the block.
	title := "┤ " + truncateRunes(summary, 80) + " ├"
	width := ansi.StringWidth(title)
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			width = w
		}
	}
	if pad := width - ansi.StringWidth(title); pad > 0 {
		title += strings.Repeat("─", pad)
	}
	var b strings.Builder
	if failed {
		b.WriteString(styleError.Render(title))
	} else {
		b.WriteString(styleBase.Render(title))
	}
	for _, l := range lines {
		b.WriteString("\n" + styleMuted.Render(l))
	}
	if cut > 0 {
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("(%d earlier lines)", cut)))
	}
	return bashBlockStyle.Render(b.String())
}

// collapseOutput keeps the tail of a tool output: at most 10 lines,
// with the number of lines cut off the head. Each line is capped at
// 200 runes, so one huge line cannot flood the scrollback.
func collapseOutput(out string) (lines []string, cut int) {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil, 0
	}
	all := strings.Split(out, "\n")
	if len(all) > 10 {
		cut = len(all) - 10
		all = all[len(all)-10:]
	}
	for _, l := range all {
		lines = append(lines, truncateRunes(l, 200))
	}
	return lines, cut
}

// truncateRunes caps s at n runes, marking the cut with "…".
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// showThinking reports whether the Thinking row may appear yet: only
// once the turn has been working for spinnerDelay. A zero start time
// (a model built by hand in tests, with working set directly) counts
// as elapsed, so old tests keep their meaning.
func (m Model) showThinking() bool {
	return m.workingSince.IsZero() || time.Since(m.workingSince) >= spinnerDelay
}

// toolPendingLabel is the running row's label for a tool — opencode's
// literal pending phrases, with the concrete object (Claude Code's
// getActivityDescription): "Reading src/foo.ts…" instead of a bare
// "Reading file…". Args that do not parse fall back to the generic
// phrase, so a label is never empty.
func toolPendingLabel(name, argsJSON string) string {
	var args map[string]json.RawMessage
	str := func(k string) string { return "" }
	if json.Unmarshal([]byte(argsJSON), &args) == nil {
		str = func(k string) string {
			var s string
			if raw, ok := args[k]; ok {
				_ = json.Unmarshal(raw, &s)
			}
			return s
		}
	}
	switch name {
	case "read":
		if p := str("path"); p != "" {
			return "Reading " + p + "…"
		}
		return "Reading file…"
	case "write":
		if p := str("path"); p != "" {
			return "Preparing write " + p + "…"
		}
		return "Preparing write…"
	case "edit":
		if p := str("path"); p != "" {
			return "Preparing edit " + p + "…"
		}
		return "Preparing edit…"
	case "grep":
		if p := str("pattern"); p != "" {
			if d := str("path"); d != "" {
				return fmt.Sprintf("Searching %q in %s…", p, d)
			}
			return fmt.Sprintf("Searching %q…", p)
		}
		return "Searching content…"
	case "glob":
		if p := str("pattern"); p != "" {
			return fmt.Sprintf("Finding %q…", p)
		}
		return "Finding files…"
	case "bash":
		if c := str("command"); c != "" {
			return "Running " + truncateRunes(c, 60) + "…"
		}
		return "Running command…"
	case "load_skill":
		if n := str("name"); n != "" {
			return "Loading skill " + n + "…"
		}
		return "Loading skill…"
	default:
		return name + "…"
	}
}

// toolSummary builds the one-line summary for a finished tool call
// from its JSON args. Match counts for grep/glob are not in the event,
// so no "(N matches)" suffix in phase 1 — counts are never invented.
// Falls back to the tool name when the args do not parse.
func toolSummary(name, argsJSON string) string {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(argsJSON), &args) == nil {
		str := func(k string) string {
			var s string
			if raw, ok := args[k]; ok {
				_ = json.Unmarshal(raw, &s)
			}
			return s
		}
		switch name {
		case "read":
			if p := str("path"); p != "" {
				return "Read " + p
			}
		case "write":
			if p := str("path"); p != "" {
				return "Write " + p
			}
		case "edit":
			if p := str("path"); p != "" {
				return "Edit " + p
			}
		case "grep":
			if p := str("pattern"); p != "" {
				if d := str("path"); d != "" {
					return fmt.Sprintf("Grep %q in %s", p, d)
				}
				return fmt.Sprintf("Grep %q", p)
			}
		case "glob":
			if p := str("pattern"); p != "" {
				return fmt.Sprintf("Glob %q", p)
			}
		case "bash":
			if c := str("command"); c != "" {
				return "$ " + c
			}
		case "load_skill":
			if n := str("name"); n != "" {
				return "Load skill " + n
			}
		}
	}
	return name
}

// toolIcon is the per-tool icon for the approval dialog.
func toolIcon(name string) string {
	switch name {
	case "read":
		return "→"
	case "write", "edit":
		return "←"
	case "grep", "glob":
		return "✱"
	case "bash":
		return "│"
	default:
		return "→"
	}
}

// stylePrint colours runner output for the scrollback. opencode shows
// no brackets in the timeline: usage lines become one muted line with
// "·" separators and thousands separators, errors become "✗ <text>".
// The runner's prints often start with a blank line ("\n[usage] …",
// "\n[error] …"), so the prefix check runs per line after stripping
// leading newlines — otherwise nothing here would ever match. Plain
// mode keeps the brackets; only the TUI strips them.
func stylePrint(s string) string {
	lead := s[:len(s)-len(strings.TrimLeft(s, "\n"))]
	rest := strings.TrimLeft(s, "\n")
	lines := strings.Split(rest, "\n")
	for i, l := range lines {
		lines[i] = stylePrintLine(l)
	}
	return lead + strings.Join(lines, "\n")
}

func stylePrintLine(l string) string {
	switch {
	case strings.HasPrefix(l, "[usage]"):
		return styleMuted.Render("   " + formatUsage(l))
	case strings.HasPrefix(l, "[compact]"):
		return styleMuted.Render("   " + strings.TrimPrefix(l, "[compact] "))
	case strings.HasPrefix(l, "[error]"):
		return styleError.Render("   ✗ " + strings.TrimPrefix(l, "[error] "))
	default:
		return l
	}
}

var usageRe = regexp.MustCompile(`^(turn|session): prompt=(\d+) \(cached=(\d+)\) completion=(\d+) total=(\d+)`)

// formatUsage restyles one runner usage line:
// "[usage] turn: prompt=3922 (cached=1920) completion=215 total=4137
//
//	| est. cost: ~$0.0007 (rates: …)" becomes
//
// "turn · prompt 3,922 (cached 1,920) · completion 215 · total 4,137
//
//	· est. cost: ~$0.0007 (rates: …)". The cost/fee suffix is kept
//
// verbatim (the rate source label must survive); an unrecognised
// shape is returned unchanged apart from the stripped prefix.
func formatUsage(line string) string {
	rest := strings.TrimPrefix(line, "[usage] ")
	head, tail := rest, ""
	if i := strings.Index(rest, " | "); i >= 0 {
		head, tail = rest[:i], rest[i+3:]
	}
	m := usageRe.FindStringSubmatch(head)
	if m == nil {
		return rest
	}
	nums := func(s string) string {
		n, err := strconv.Atoi(s)
		if err != nil {
			return s
		}
		return formatInt(n)
	}
	out := fmt.Sprintf("%s · prompt %s (cached %s) · completion %s · total %s",
		m[1], nums(m[2]), nums(m[3]), nums(m[4]), nums(m[5]))
	if tail != "" {
		out += " · " + tail
	}
	return out
}

// formatInt renders n with thousands separators (3,922).
func formatInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// View renders only the live region: the streaming text with the
// current tool, and the input line (or the approval dialog, which
// replaces the input line while it is pending). The header was
// printed once at startup (see Init); finished turns are already in
// scrollback via tea.Println. The cursor is placed explicitly on the
// input line, because the meta line sits below it.
func (m Model) View() tea.View {
	var liveLines []string
	if m.live != "" {
		liveLines = append(liveLines, strings.Split(renderText(m.live), "\n")...)
	}
	if m.liveTool != "" {
		liveLines = append(liveLines, "   "+styleMuted.Render(spinnerGlyph(m.spinnerFrame))+" "+styleMuted.Render(toolPendingLabel(m.liveTool, m.liveToolArgs)))
	} else if m.working && m.live == "" && m.showThinking() {
		liveLines = append(liveLines, "   "+styleWarning.Render(spinnerGlyph(m.spinnerFrame))+" "+styleWarning.Render("Thinking…"))
	}
	var lines []string
	lines = append(lines, liveLines...)
	if m.approval != nil {
		lines = append(lines, m.approvalBox())
	} else {
		lines = append(lines, m.inputLine())
		if meta := m.metaLine(); meta != "" {
			lines = append(lines, meta)
		}
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	if m.approval == nil {
		// Cursor on the input line, after the typed prefix. The "┃ "
		// prefix is 2 cells; the prefix width is measured in display
		// cells, not runes (CJK input is 2 cells per rune).
		x := 2 + ansi.StringWidth(string(m.input[:m.cursor]))
		v.Cursor = tea.NewCursor(x, len(liveLines))
	}
	return v
}

// inputLine renders the composer: a heavy left border (peach, dimmed
// to the border colour while a turn runs), the typed text or the
// placeholder, on the raised background.
func (m Model) inputLine() string {
	barColor := colorPeach
	if m.working {
		barColor = colorBorder
	}
	bar := lipgloss.NewStyle().Foreground(barColor).Render("┃")
	text := string(m.input)
	var rendered string
	if text == "" {
		rendered = styleMuted.Render(placeholderText(m.width))
	} else {
		rendered = styleBase.Render(text)
	}
	st := raisedBackground(lipgloss.NewStyle())
	if m.width > 0 {
		st = st.Width(m.width)
	}
	return st.Render(bar + " " + rendered)
}

// placeholderText is the empty-input hint; it replaces the startup
// help line. Truncated to the terminal width when known.
func placeholderText(width int) string {
	const ph = "Ask anything…  /skill:<name> /usage /jobs /clear /compact /quit"
	if width <= 2 {
		return ph
	}
	avail := width - 2 // the "┃ " prefix
	return truncateRunes(ph, avail)
}

// metaLine is the composer meta line under the input, split
// left/right like a statusline (helix): left is the mode chip (dark
// text on the accent colour, yazi's mode chip) · model · api; right,
// when known, is the git branch and the session totals (turns ·
// tokens). Empty when the meta was never set (tests).
func (m Model) metaLine() string {
	if m.metaMode == "" && m.metaModel == "" && m.metaAPI == "" {
		return ""
	}
	left := styleModeChip.Render(m.metaMode) + styleMuted.Render(" · ") +
		styleBase.Render(m.metaModel) + styleMuted.Render(" · ") +
		styleMuted.Render(m.metaAPI)
	var right string
	if m.metaBranch != "" {
		right = styleMuted.Render(m.metaBranch)
	}
	if m.turnCount > 0 || m.sessionTokens > 0 {
		turnWord := "turns"
		if m.turnCount == 1 {
			turnWord = "turn"
		}
		totals := fmt.Sprintf("%d %s · %s tokens", m.turnCount, turnWord, formatInt(m.sessionTokens))
		if right != "" {
			right += styleMuted.Render(" · ") + styleMuted.Render(totals)
		} else {
			right = styleMuted.Render(totals)
		}
	}
	line := "  " + left
	if right != "" {
		// Right-align when the width is known and everything fits;
		// otherwise fall back to joining with a separator.
		gap := 0
		if m.width > 0 {
			gap = m.width - 2 - ansi.StringWidth(left) - ansi.StringWidth(right)
		}
		if gap >= 2 {
			line += strings.Repeat(" ", gap) + right
		} else {
			line += styleMuted.Render(" · ") + right
		}
	}
	st := raisedBackground(lipgloss.NewStyle())
	if m.width > 0 {
		st = st.Width(m.width)
	}
	return st.Render(line)
}

// userPanel renders a submitted user turn as a raised panel with a
// peach left edge, for the scrollback.
func userPanel(line string, width int) string {
	st := raisedBackground(lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
		BorderForeground(colorPeach).
		Foreground(lipgloss.Color("#eeeeee")).
		Padding(0, 1))
	if width > 0 {
		st = st.Width(width)
	}
	return st.Render(line)
}

// approvalBox renders the pending approval as opencode's permission
// panel: a full-width block with a peach left border (not a box), a
// warning "△ Approval required" header, the tool line, the raw detail,
// and the option row. It stays inline-first (no alt-screen overlay).
func (m Model) approvalBox() string {
	width := 60
	if m.width > 8 && m.width-4 < width {
		width = m.width - 4
	}
	if width < 20 {
		width = 20
	}
	// lipgloss Width() is the TOTAL block width; the text area
	// inside is the left border (1) plus the left+right padding
	// (2+2) narrower. Wrap and right-align against that text width,
	// or the option row wraps (a row exactly text-wide already
	// pushes its last word onto the next line).
	textWidth := width - 5
	if textWidth < 10 {
		textWidth = 10
	}
	tool, summary, detail := parseApprovalPrompt(m.approval.Prompt)
	header := styleWarning.Render("△") + " " + styleTitle.Render("Approval required")
	toolLine := "  " + styleMuted.Render(toolIcon(tool)) + " " + styleBase.Render(summary)
	detailLines := wrapText(detail, textWidth)
	// Option row: the selected option has a solid blue background
	// (the colour the opencode screenshots show for the focused
	// button); the hint sits at the right edge.
	options := styleSelect.Render("[y] Allow once") + "   " + styleMuted.Render("[n] Deny")
	hint := "esc = deny"
	// Keep the row a touch short of the edge: a row exactly
	// text-wide wraps in lipgloss (the last word lands on the next
	// line).
	optsLen := len([]rune("[y] Allow once")) + 3 + len([]rune("[n] Deny"))
	gap := textWidth - optsLen - len([]rune(hint)) - 2
	if gap < 3 {
		gap = 3
	}
	optionRow := options + strings.Repeat(" ", gap) + styleMuted.Render(hint)
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n" + toolLine)
	for _, l := range detailLines {
		b.WriteString("\n" + styleMuted.Render(l))
	}
	b.WriteString("\n" + optionRow)
	st := raisedBackground(lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
		BorderForeground(colorPeach).
		Padding(1, 2).
		Width(width))
	return st.Render(b.String())
}

// parseApprovalPrompt splits an approval prompt "Run <tool> <json>?"
// into the tool name, a one-line summary and the detail (the raw
// JSON). It falls back to the raw prompt when the shape is
// unexpected, so the dialog never hides what it is asking about.
func parseApprovalPrompt(prompt string) (tool, summary, detail string) {
	rest := strings.TrimSuffix(strings.TrimPrefix(prompt, "Run "), "?")
	i := strings.Index(rest, " ")
	if i < 0 {
		return "", prompt, prompt
	}
	tool = rest[:i]
	detail = rest[i+1:]
	summary = toolSummary(tool, detail)
	if summary == tool {
		summary = prompt
	}
	return tool, summary, detail
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
