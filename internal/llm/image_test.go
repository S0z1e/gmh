package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatWireMessageWithImages(t *testing.T) {
	img := Image{Path: "pic.png", MediaType: "image/png", Data: "aGVsbG8=", Width: 2, Height: 2, Bytes: 5}
	m := Message{Role: "user", Content: "what is this?", Images: []Image{img}}
	b, err := json.Marshal(chatRequest{Model: "m", Messages: toChatWireMessages([]Message{m}), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	msgs := req["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content parts = %d, want text + image", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "what is this?" {
		t.Fatalf("text part = %v", content[0])
	}
	imagePart := content[1].(map[string]any)
	if imagePart["type"] != "image_url" {
		t.Fatalf("image part type = %v", imagePart["type"])
	}
	url := imagePart["image_url"].(map[string]any)["url"].(string)
	if url != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image url = %q", url)
	}
	// Without images the content stays a plain string, exactly as before.
	plain := toChatWireMessage(Message{Role: "user", Content: "hi"})
	b, _ = json.Marshal(plain)
	if !strings.Contains(string(b), `"content":"hi"`) {
		t.Fatalf("plain message content = %s, want a string", b)
	}
	// Images with no bytes (e.g. a resumed session whose file is
	// gone) are skipped, never sent as empty data URIs.
	empty := toChatWireMessage(Message{Role: "user", Content: "hi", Images: []Image{{Path: "gone.png", MediaType: "image/png"}}})
	b, _ = json.Marshal(empty)
	if strings.Contains(string(b), "image_url") {
		t.Fatalf("empty-data image must be skipped: %s", b)
	}
}

func TestToAnthropicMessagesWithImages(t *testing.T) {
	img := Image{Path: "pic.png", MediaType: "image/png", Data: "aGVsbG8=", Width: 2, Height: 2, Bytes: 5}
	_, msgs := toAnthropicMessages([]Message{{Role: "user", Content: "what is this?", Images: []Image{img}}})
	if len(msgs) != 1 || len(msgs[0].Content) != 2 {
		t.Fatalf("messages = %+v", msgs)
	}
	blk := msgs[0].Content[0]
	if blk.Type != "image" || blk.Source == nil || blk.Source.Type != "base64" || blk.Source.MediaType != "image/png" || blk.Source.Data != "aGVsbG8=" {
		t.Fatalf("image block = %+v", blk)
	}
	if msgs[0].Content[1].Type != "text" || msgs[0].Content[1].Text != "what is this?" {
		t.Fatalf("text block = %+v", msgs[0].Content[1])
	}
}

func TestToResponsesInputWithImages(t *testing.T) {
	img := Image{Path: "pic.png", MediaType: "image/png", Data: "aGVsbG8="}
	_, input := toResponsesInput([]Message{{Role: "user", Content: "what is this?", Images: []Image{img}}})
	if len(input) != 1 {
		t.Fatalf("input = %v", input)
	}
	item := input[0].(map[string]any)
	parts := item["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %v", parts)
	}
	if parts[0].(map[string]any)["type"] != "input_text" {
		t.Fatalf("part 0 = %v", parts[0])
	}
	if parts[1].(map[string]any)["type"] != "input_image" || parts[1].(map[string]any)["image_url"] != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("part 1 = %v", parts[1])
	}
	// Text-only messages keep exactly one input_text part.
	_, input = toResponsesInput([]Message{{Role: "user", Content: "hi"}})
	parts = input[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("text-only parts = %v", parts)
	}
}

func TestImageDescribeAndDataURL(t *testing.T) {
	img := Image{Path: "a/b.png", MediaType: "image/png", Data: "xyz", Width: 400, Height: 300, Bytes: 2899}
	if img.Describe() != "a/b.png (400x300, 2899 bytes)" {
		t.Fatalf("describe = %q", img.Describe())
	}
	if img.DataURL() != "data:image/png;base64,xyz" {
		t.Fatalf("data URL = %q", img.DataURL())
	}
	nodims := Image{Path: "w.webp", MediaType: "image/webp", Bytes: 100}
	if nodims.Describe() != "w.webp (100 bytes)" {
		t.Fatalf("describe without dims = %q", nodims.Describe())
	}
	// Data never reaches the session file: it is json:"-".
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "xyz") {
		t.Fatalf("session JSON must not carry image bytes: %s", b)
	}
	if !strings.Contains(string(b), `"path":"a/b.png"`) {
		t.Fatalf("session JSON must carry the path: %s", b)
	}
}
