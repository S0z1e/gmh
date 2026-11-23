package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTextAttachmentWithImageInProviderRequests(t *testing.T) {
	m := Message{
		Role: "user", Content: "Compare the screenshot and log",
		Files:  []TextAttachment{{Path: "server.log", Content: "ERROR: missing socket\r\n"}},
		Images: []Image{{Path: "screen.png", MediaType: "image/png", Data: "aGVsbG8="}},
	}
	_, responses := toResponsesInput([]Message{m})
	_, anthropic := toAnthropicMessages([]Message{m})
	for name, wire := range map[string]any{"chat": toChatWireMessage(m), "responses": responses, "anthropic": anthropic} {
		b, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{m.Content, "server.log", `ERROR: missing socket\r\n`, "aGVsbG8="} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s request lacks %q: %s", name, want, b)
			}
		}
	}
}
