package tui

import (
	"strings"
	"testing"
)

func TestDefaultThemeValues(t *testing.T) {
	d := DefaultTheme()
	if d.Peach != "#fab283" || d.Raised != "#141414" || d.Text != "#eeeeee" {
		t.Fatalf("default theme = %+v", d)
	}
	if got := ThemeByName(""); got != d {
		t.Fatalf("empty theme name = %+v, want default", got)
	}
	if got := ThemeByName("nope"); got != d {
		t.Fatalf("unknown theme name = %+v, want default", got)
	}
}

func TestTerminalThemePaintsNoBackground(t *testing.T) {
	// Positive control: the default theme paints a raised background.
	if got := userPanel("x", 0); !strings.Contains(got, "48;") {
		t.Fatalf("default theme should paint a background, got %q", got)
	}
	SetTheme(TerminalTheme())
	t.Cleanup(func() { SetTheme(DefaultTheme()) })
	if got := userPanel("x", 0); strings.Contains(got, "48;") {
		t.Fatalf("terminal theme should paint no background, got %q", got)
	}
	// The mode chip keeps its accent background: it is a badge, not a panel.
	if got := styleModeChip.Render("auto"); !strings.Contains(got, "48;") {
		t.Fatalf("mode chip should keep its background, got %q", got)
	}
}

func TestCatppuccinMochaThemeValues(t *testing.T) {
	c := CatppuccinMochaTheme()
	if c.Raised != "#181825" || c.Text != "#cdd6f4" || c.Peach != "#fab387" || c.Purple != "#cba6f7" {
		t.Fatalf("catppuccin theme = %+v", c)
	}
	if got := ThemeByName("catppuccin-mocha"); got != c {
		t.Fatalf("theme by name = %+v, want %+v", got, c)
	}
}
