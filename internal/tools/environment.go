package tools

import (
	"os"
	"runtime"
	"strings"
)

var defaultBashEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TERM",
	"TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMFILES",
}

func bashEnvironment(allow []string) []string {
	if allow == nil {
		allow = defaultBashEnv
	}
	normalize := func(name string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(name)
		}
		return name
	}
	names := make(map[string]bool, len(allow))
	for _, name := range allow {
		names[normalize(name)] = true
	}
	// A non-nil empty slice is essential: nil would inherit the whole parent.
	result := make([]string, 0, len(allow))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && names[normalize(name)] {
			result = append(result, entry)
		}
	}
	return result
}
