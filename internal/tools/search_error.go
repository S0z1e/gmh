package tools

import (
	"fmt"
	"strings"
)

// Keep diagnostics bounded independently of the search's stdout limit. An exit
// status alone cannot distinguish malformed input from unreadable files.
type searchDiagnostics struct{ data []byte }

func (d *searchDiagnostics) Write(p []byte) (int, error) {
	const limit = 8 << 10
	if len(d.data) < limit {
		d.data = append(d.data, p[:min(len(p), limit-len(d.data))]...)
	}
	return len(p), nil
}

func searchError(binary string, err error, diagnostics searchDiagnostics) error {
	wrapped := fmt.Errorf("%s failed: %w", binary, err)
	text := strings.TrimSpace(string(diagnostics.data))
	invalid := strings.HasPrefix(text, "regex parse error:") || strings.HasPrefix(text, "rg: regex parse error:") || strings.HasPrefix(text, "rg: error parsing glob ")
	if binary == "grep" && strings.HasPrefix(text, "grep:") {
		// BSD and GNU grep report their own BRE parser diagnostics.
		message := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "grep:")))
		for _, marker := range []string{
			"brackets ([ ]) not balanced", "parentheses not balanced", "braces not balanced",
			"invalid regular expression", "unmatched [", "unmatched (", `unmatched \{`,
			"trailing backslash", "repetition-operator operand invalid", "invalid repetition count(s)",
			"invalid character range", "invalid character class", "invalid collating element",
			"invalid backreference number", "invalid range end", "invalid back reference", "invalid back-reference",
			`invalid content of \{\}`, "invalid collation character",
		} {
			invalid = invalid || strings.HasPrefix(message, marker)
		}
	}
	if invalid {
		return &InputError{Err: fmt.Errorf("%w\n%s", wrapped, text)}
	}
	return wrapped
}
