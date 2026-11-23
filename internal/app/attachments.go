package app

import (
	"fmt"
	"slices"

	"minimalharness/internal/llm"
	"minimalharness/internal/tools"
)

const maxFilesPerMessage = 10

// AttachFile queues a snapshot of a text file for the next task.
func (a *App) AttachFile(path string) (llm.TextAttachment, error) {
	if len(a.PendingFiles) >= maxFilesPerMessage {
		return llm.TextAttachment{}, fmt.Errorf("at most %d text attachments per message", maxFilesPerMessage)
	}
	file, err := a.Cfg.Exec.LoadTextAttachment(path)
	if err != nil {
		return llm.TextAttachment{}, err
	}
	total := len(file.Content)
	for _, pending := range a.PendingFiles {
		total += len(pending.Content)
	}
	if total > tools.MaxTextAttachmentBytes {
		return llm.TextAttachment{}, fmt.Errorf("text attachments may total at most %d bytes per message", tools.MaxTextAttachmentBytes)
	}
	pending := slices.Concat(a.PendingFiles, []llm.TextAttachment{file})
	if err := a.checkAttachmentContext(llm.Message{Role: "user", Kind: "task", Images: a.PendingImages, Files: pending}); err != nil {
		return llm.TextAttachment{}, err
	}
	a.PendingFiles = pending
	return file, nil
}

func (a *App) checkAttachmentContext(task llm.Message) error {
	if len(task.Files) > 0 && !a.Cfg.FreshTurnFits(task, a.turnContext("")) {
		return fmt.Errorf("text attachments and task exceed the model's input budget; use smaller attachments or select a larger context window")
	}
	return nil
}

func (r *Runner) handleAttach(arg string) {
	if arg == "" {
		if len(r.App.PendingFiles) == 0 {
			r.Print("No text files attached. Usage: /attach <path>.")
		}
		for _, file := range r.App.PendingFiles {
			r.Print("Attached: %q (%d bytes)", file.Path, len(file.Content))
		}
		return
	}
	file, err := r.App.AttachFile(arg)
	if err != nil {
		r.Print("[error] %v", err)
		return
	}
	r.Print("Attached %q (%d bytes) to your next message.", file.Path, len(file.Content))
}
