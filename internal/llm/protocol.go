package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

// ProtocolError rejects a corrupted reply before onDone can publish a message
// that the agent could persist or execute. Retrying is safe before onDone.
type ProtocolError struct{ API, Detail string }

func (e *ProtocolError) Error() string { return e.API + " protocol error: " + e.Detail }

func protocolError(api, format string, args ...any) error {
	return &ProtocolError{API: api, Detail: fmt.Sprintf(format, args...)}
}

func decodeStreamJSON(api, data string, dst any) error {
	if !strings.HasPrefix(strings.TrimSpace(data), "{") {
		return protocolError(api, "SSE data must be a JSON object")
	}
	if err := json.Unmarshal([]byte(data), dst); err != nil {
		return protocolError(api, "invalid SSE JSON: %v", err)
	}
	return nil
}

func sameJSON(a, b string) bool {
	var x, y any
	dx, dy := json.NewDecoder(strings.NewReader(a)), json.NewDecoder(strings.NewReader(b))
	dx.UseNumber()
	dy.UseNumber()
	if dx.Decode(&x) != nil || dy.Decode(&y) != nil || !json.Valid([]byte(a)) || !json.Valid([]byte(b)) {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func completeStream(api string, msg Message, usage Usage, done func(Message, Usage)) error {
	switch msg.FinishReason {
	case "length", "max_tokens", "incomplete", "content_filter":
		// Preserve text and finish metadata, never an unfinished action.
		msg.ToolCalls = nil
		var blocks []AnthropicContentBlock
		for _, b := range msg.AnthropicContent {
			if b.Type != "tool_use" {
				blocks = append(blocks, b)
			}
		}
		msg.AnthropicContent = blocks
	default:
		seen := map[string]bool{}
		for _, tc := range msg.ToolCalls {
			if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Function.Name) == "" {
				return protocolError(api, "tool call is missing its id or name")
			}
			if seen[tc.ID] {
				return protocolError(api, "duplicate tool call id %q", tc.ID)
			}
			seen[tc.ID] = true
			if tc.Type != "" && tc.Type != "function" {
				return protocolError(api, "unsupported tool call type %q", tc.Type)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &object); err != nil || object == nil {
				return protocolError(api, "tool call %q arguments must be a complete JSON object", tc.ID)
			}
		}
	}
	if done != nil {
		done(msg, usage)
	}
	return nil
}

const maxSSEEventBytes = 4 << 20

// Unlike ScanLines, SSE accepts CR, LF and CRLF, even across reads.
func splitSSELines() bufio.SplitFunc {
	skipLF := false
	return func(data []byte, atEOF bool) (int, []byte, error) {
		if len(data) == 0 {
			return 0, nil, nil
		}
		if skipLF {
			skipLF = false
			if data[0] == '\n' {
				return 1, nil, nil
			}
		}
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			skipLF = data[i] == '\r'
			return i + 1, data[:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
}

// SSE data fields form one event at a blank line, with embedded newlines.
// EOF does not dispatch pending data (WHATWG event-stream interpretation).
// https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation
type sseScanner struct {
	lines *idleLineScanner
	api   string
	first bool
}

func newSSEScanner(body io.Reader, idle time.Duration, model, api string) *sseScanner {
	return &sseScanner{lines: newIdleLineScanner(body, idle, model), api: api, first: true}
}

func (s *sseScanner) close() { s.lines.close() }

func (s *sseScanner) next() (string, bool, error) {
	var data strings.Builder
	hasData := false
	for {
		line, ok, err := s.lines.next()
		if !ok {
			if err != nil {
				if errors.Is(err, bufio.ErrTooLong) {
					return "", false, protocolError(s.api, "SSE line exceeds size limit")
				}
				return "", false, err
			}
			if hasData {
				return "", false, protocolError(s.api, "stream ended inside an SSE event")
			}
			return "", false, nil
		}
		if s.first {
			line = strings.TrimPrefix(line, "\ufeff")
			s.first = false
		}
		if line == "" {
			if hasData {
				return strings.TrimSuffix(data.String(), "\n"), true, nil
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		} // Comments, event names and extensions.
		value = strings.TrimPrefix(value, " ")
		if data.Len()+len(value)+1 > maxSSEEventBytes {
			return "", false, protocolError(s.api, "SSE event exceeds %d-byte limit", maxSSEEventBytes)
		}
		data.WriteString(value)
		data.WriteByte('\n')
		hasData = true
	}
}
