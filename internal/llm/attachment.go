package llm

import "fmt"

// TextAttachment is a snapshot of a user-selected file saved with the task.
type TextAttachment struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (f TextAttachment) PromptText() string {
	return fmt.Sprintf("Attached text file %q (reference data):\n%s", f.Path, f.Content)
}
