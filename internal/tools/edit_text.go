package tools

import "strings"

// replaceNormalizedRange maps LF-normalized byte offsets back to the original
// text. Only the replacement adopts the local line ending; all other bytes stay.
func replaceNormalizedRange(raw string, start, end int, newText string) []byte {
	start = rawTextOffset(raw, start)
	end = rawTextOffset(raw, end)
	newline := strings.IndexByte(raw[start:], '\n')
	if newline >= 0 {
		newline += start
	} else {
		newline = strings.LastIndexByte(raw[:start], '\n')
	}
	if newline > 0 && raw[newline-1] == '\r' {
		newText = strings.ReplaceAll(newText, "\n", "\r\n")
	}
	return []byte(raw[:start] + newText + raw[end:])
}

// rawTextOffset accounts for the extra carriage return in each CRLF pair.
func rawTextOffset(raw string, normalized int) int {
	for i := 0; i < len(raw); i++ {
		if normalized == 0 {
			return i
		}
		if raw[i] == '\r' && i+1 < len(raw) && raw[i+1] == '\n' {
			i++
		}
		normalized--
	}
	return len(raw)
}
