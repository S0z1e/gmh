// Theme support for the TUI: the palette lives in one Theme struct,
// and every lipgloss style is built from it by SetTheme. Adding a
// theme is filling in a struct — no layout code changes.
//
// The default theme is the opencode look (dark #0a0a0a background,
// peach accent). The "terminal" theme paints no raised background, so
// the user's own terminal background shows through (Crush does the
// same). Catppuccin Mocha is a second full palette. An empty string
// in a background slot means "don't paint it".
package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Theme is one palette: every colour the TUI paints, as hex strings.
// A background slot left empty is not painted at all.
type Theme struct {
	Raised   string // raised panels, input box, tool blocks
	Border   string // left-edge borders
	Text     string // main text
	Muted    string // secondary text
	Peach    string // interactive accent
	Purple   string // headings, keywords
	Quote    string // blockquotes, emphasis
	Code     string // inline code, code blocks
	CodeBg   string // code block background
	Error    string
	Warning  string
	Success  string
	SelectBg string // focused option background (approval dialog)
	SelectFg string // focused option foreground
}

// DefaultTheme is the opencode look the TUI shipped with.
func DefaultTheme() Theme {
	return Theme{
		Raised:   "#141414",
		Border:   "#484848",
		Text:     "#eeeeee",
		Muted:    "#808080",
		Peach:    "#fab283",
		Purple:   "#9d7cd8",
		Quote:    "#e5c07b",
		Code:     "#7fd88f",
		CodeBg:   "#141414",
		Error:    "#e06c75",
		Warning:  "#f5a742",
		Success:  "#7fd88f",
		SelectBg: "#5c9cf5",
		SelectFg: "#0a0a0a",
	}
}

// TerminalTheme follows the terminal instead of painting its own
// raised background: panels, the input box and tool blocks stay
// transparent, so any terminal theme (light or dark) shows through
// without clashing. Text colours are the default ones — they assume
// a dark terminal, as Crush's terminal theme does.
func TerminalTheme() Theme {
	t := DefaultTheme()
	t.Raised = ""
	t.CodeBg = ""
	return t
}

// CatppuccinMochaTheme is the Catppuccin Mocha palette, verified
// against the official palette: base #1e1e2e, mantle #181825,
// text #cdd6f4, subtext0 #a6adc8, overlay0 #6c7086, mauve #cba6f7,
// blue #89b4fa, peach #fab387, green #a6e3a1, yellow #f9e2af,
// red #f38ba8. Its peach is nearly the default accent, so switching
// themes does not change the feel abruptly.
func CatppuccinMochaTheme() Theme {
	return Theme{
		Raised:   "#181825",
		Border:   "#6c7086",
		Text:     "#cdd6f4",
		Muted:    "#a6adc8",
		Peach:    "#fab387",
		Purple:   "#cba6f7",
		Quote:    "#f9e2af",
		Code:     "#a6e3a1",
		CodeBg:   "#181825",
		Error:    "#f38ba8",
		Warning:  "#f9e2af",
		Success:  "#a6e3a1",
		SelectBg: "#89b4fa",
		SelectFg: "#1e1e2e",
	}
}

// ThemeByName picks a theme by name: "default" (or ""), "terminal",
// "catppuccin-mocha". Unknown names fall back to the default.
func ThemeByName(name string) Theme {
	switch name {
	case "terminal":
		return TerminalTheme()
	case "catppuccin-mocha":
		return CatppuccinMochaTheme()
	default:
		return DefaultTheme()
	}
}

var currentTheme = DefaultTheme()

// Palette colours and styles, rebuilt by SetTheme. Everything in
// this package renders through these, so a theme switch is one call.
var (
	colorRaised color.Color
	colorBorder color.Color
	colorPeach  color.Color
	colorPurple color.Color
	colorQuote  color.Color

	styleBase    lipgloss.Style
	styleMuted   lipgloss.Style
	styleTitle   lipgloss.Style
	stylePeach   lipgloss.Style
	stylePurple  lipgloss.Style
	styleBold    lipgloss.Style
	styleCode    lipgloss.Style
	styleCodeBlk lipgloss.Style
	styleQuote   lipgloss.Style
	styleError   lipgloss.Style
	styleWarning lipgloss.Style
	styleSuccess lipgloss.Style
	styleSelect  lipgloss.Style
	// styleModeChip is the mode badge on the meta line: dark text on
	// the accent colour (yazi's mode chip), not just coloured text.
	styleModeChip lipgloss.Style
	// badgeStyle is the muted-on-raised "Background" badge.
	badgeStyle lipgloss.Style
	// bashBlockStyle is the bash block's left border + raised background.
	bashBlockStyle lipgloss.Style
)

func init() { SetTheme(DefaultTheme()) }

// SetTheme rebuilds every colour and style from t. Call it once at
// startup, before NewProgram; rendering after that uses the new theme.
func SetTheme(t Theme) {
	currentTheme = t
	colorRaised = lipgloss.Color(t.Raised)
	colorBorder = lipgloss.Color(t.Border)
	colorPeach = lipgloss.Color(t.Peach)
	colorPurple = lipgloss.Color(t.Purple)
	colorQuote = lipgloss.Color(t.Quote)

	styleBase = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text))
	styleMuted = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Muted))
	styleTitle = lipgloss.NewStyle().Bold(true)
	stylePeach = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Peach))
	stylePurple = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Purple)).Bold(true)
	styleBold = lipgloss.NewStyle().Bold(true)
	styleCode = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Code))
	styleCodeBlk = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Code))
	if t.CodeBg != "" {
		styleCodeBlk = styleCodeBlk.Background(lipgloss.Color(t.CodeBg))
	}
	styleQuote = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Quote))
	styleError = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Error)).Bold(true)
	styleWarning = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Warning))
	styleSuccess = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Success))
	styleSelect = lipgloss.NewStyle().Background(lipgloss.Color(t.SelectBg)).Foreground(lipgloss.Color(t.SelectFg)).Bold(true)
	styleModeChip = lipgloss.NewStyle().Background(lipgloss.Color(t.Peach)).Foreground(lipgloss.Color(t.SelectFg)).Bold(true).Padding(0, 1)
	badgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Muted))
	if t.Raised != "" {
		badgeStyle = badgeStyle.Background(lipgloss.Color(t.Raised))
	}
	bashBlockStyle = lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
		BorderForeground(colorBorder).
		Padding(0, 1)
	if t.Raised != "" {
		bashBlockStyle = bashBlockStyle.Background(lipgloss.Color(t.Raised))
	}
}

// raisedBackground adds the raised background to st, unless the
// theme leaves it unpainted (the "terminal" theme). Use it wherever
// a style is built inline instead of in SetTheme.
func raisedBackground(st lipgloss.Style) lipgloss.Style {
	if currentTheme.Raised != "" {
		return st.Background(colorRaised)
	}
	return st
}
