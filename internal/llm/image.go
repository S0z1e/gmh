package llm

import (
	"fmt"
	"strings"
)

// Image is one attached image. Path + MediaType are the identity: the
// session file stores only those (plus dimensions), never the bytes,
// and the bytes are re-read from disk on --resume. Data holds the
// base64 bytes in memory only (json:"-" keeps them out of the file).
type Image struct {
	Path string `json:"path"`
	// Workspace marks images chosen by a tool, which stay jailed on reload.
	Workspace bool   `json:"workspace,omitempty"`
	MediaType string `json:"media_type"`
	Data      string `json:"-"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
}

// DataURL renders the image as a data: URI for the OpenAI wire
// formats (chat image_url, Responses input_image).
func (img Image) DataURL() string {
	return "data:" + img.MediaType + ";base64," + img.Data
}

// Describe renders the image for humans: path, dimensions when
// known, byte size.
func (img Image) Describe() string {
	if img.Width > 0 && img.Height > 0 {
		return fmt.Sprintf("%s (%dx%d, %d bytes)", img.Path, img.Width, img.Height, img.Bytes)
	}
	return fmt.Sprintf("%s (%d bytes)", img.Path, img.Bytes)
}

// --- Chat Completions wire shape ---
//
// A message with images sends content as parts (text + image_url);
// without images the content stays a plain string, exactly as
// before, so existing requests are byte-identical.

type chatWireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// toChatWireMessage converts one shared Message to its wire form,
// skipping images that have no bytes (e.g. a session file whose
// image was deleted before --resume re-read it).
func toChatWireMessage(m Message) chatWireMessage {
	if len(m.Images) == 0 {
		return chatWireMessage{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	}
	var parts []any
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, img := range m.Images {
		if img.Data == "" {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": img.DataURL()},
		})
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	return chatWireMessage{Role: m.Role, Content: parts, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
}

// toChatWireMessages converts a whole history. Messages without
// images keep their plain-string content.
func toChatWireMessages(messages []Message) []chatWireMessage {
	out := make([]chatWireMessage, 0, len(messages))
	for _, m := range messages {
		out = append(out, toChatWireMessage(m))
	}
	return out
}

// imageParts renders the image parts of a message for the
// non-chat wire formats (Anthropic blocks, Responses input items).
// Callers decide the part shape; this only filters out images with
// no bytes.
func imageParts(m Message) []Image {
	var out []Image
	for _, img := range m.Images {
		if img.Data != "" {
			out = append(out, img)
		}
	}
	return out
}

// ImageText joins image descriptions for the short text that
// accompanies an attached image ("Image attached: ...").
func ImageText(imgs []Image) string {
	parts := make([]string, 0, len(imgs))
	for _, img := range imgs {
		parts = append(parts, img.Describe())
	}
	return "Image attached: " + strings.Join(parts, ", ") + "."
}
