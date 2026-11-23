package llm

import (
	"fmt"
	"strings"
)

// Image identifies immutable attachment bytes by digest. Path records their
// origin. Data holds the base64 bytes in memory only, outside transcript JSON.
type Image struct {
	Path        string `json:"path"`
	Digest      string `json:"digest,omitempty"`
	Unavailable string `json:"-"`
	// Workspace records that a tool selected this image inside the workspace.
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
	if img.Unavailable != "" {
		return img.Path + " (image unavailable: " + img.Unavailable + ")"
	}
	if img.Width > 0 && img.Height > 0 {
		return fmt.Sprintf("%s (%dx%d, %d bytes)", img.Path, img.Width, img.Height, img.Bytes)
	}
	return fmt.Sprintf("%s (%d bytes)", img.Path, img.Bytes)
}

// --- Chat Completions wire shape ---
//
// A message with images sends content as parts (text + image_url).
// Without images, use a plain string for compatibility with chat providers.

type chatWireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// toChatWireMessage converts one shared Message to its wire form,
// reporting unavailable images as text instead of empty image parts.
func toChatWireMessage(m Message) chatWireMessage {
	if len(m.Images) == 0 {
		content := m.Content
		if m.Role == "user" && len(m.Files) > 0 {
			var texts []string
			if content != "" {
				texts = append(texts, content)
			}
			for _, file := range m.Files {
				texts = append(texts, file.PromptText())
			}
			content = strings.Join(texts, "\n\n")
		}
		return chatWireMessage{Role: m.Role, Content: content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	}
	var parts []any
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	if m.Role == "user" {
		for _, f := range m.Files {
			parts = append(parts, map[string]any{"type": "text", "text": f.PromptText()})
		}
	}
	for _, img := range m.Images {
		if img.Data == "" {
			parts = append(parts, map[string]any{"type": "text", "text": img.UnavailableText()})
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

func (img Image) UnavailableText() string {
	reason := img.Unavailable
	if reason == "" {
		reason = "no saved image data"
	}
	return fmt.Sprintf("[Image unavailable: %s (%s)]", img.Path, reason)
}

func unavailableImageText(m Message) string {
	var parts []string
	for _, img := range m.Images {
		if img.Data == "" {
			parts = append(parts, img.UnavailableText())
		}
	}
	return strings.Join(parts, "\n")
}

// toChatWireMessages converts a whole history.
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
