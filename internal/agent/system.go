package agent

import _ "embed"

// SystemPrompt is the lightweight system prompt, embedded so the binary
// stays single-file. Edit internal/agent/prompts/system.md to change it.

//go:embed prompts/system.md
var SystemPrompt string
