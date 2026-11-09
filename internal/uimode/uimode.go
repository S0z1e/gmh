// Package uimode decides between the TUI and the plain CLI.
//
// Precedence: --ui flag > NO_COLOR > TERM=dumb > TTY.
//   - --ui plain always means plain; --ui tui always means TUI, unless
//     stdout is not a terminal (a TUI cannot run there).
//   - --ui auto (the default): NO_COLOR set to a non-empty value, or
//     TERM=dumb, or a non-terminal stdout all mean plain.
//   - -p print mode ignores --ui entirely: it is always plain.
//
// NO_COLOR follows no-color.org: it counts only when present and
// non-empty.
package uimode

import (
	"fmt"
	"os"
)

// Decide returns "tui" or "plain" for the given --ui flag value and
// whether stdout is a terminal.
func Decide(flag string, isTTY bool) (string, error) {
	switch flag {
	case "plain":
		return "plain", nil
	case "tui":
		if !isTTY {
			return "plain", nil
		}
		return "tui", nil
	case "auto", "":
		if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
			return "plain", nil
		}
		if os.Getenv("TERM") == "dumb" {
			return "plain", nil
		}
		if !isTTY {
			return "plain", nil
		}
		return "tui", nil
	default:
		return "", fmt.Errorf("invalid --ui %q: use auto, plain or tui", flag)
	}
}

// IsTTY reports whether f is a character device (a terminal or pty),
// as opposed to a pipe or a regular file.
func IsTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
