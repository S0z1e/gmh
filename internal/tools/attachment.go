package tools

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"minimalharness/internal/fileguard"
	"minimalharness/internal/llm"
)

const MaxTextAttachmentBytes = 64 << 10

// LoadTextAttachment snapshots a file the user explicitly named. Relative
// paths resolve against the workspace, and absolute paths can be outside it.
func (e *Executor) LoadTextAttachment(path string) (llm.TextAttachment, error) {
	if path == "" {
		return llm.TextAttachment{}, fmt.Errorf("attach_file requires a file path")
	}
	diskPath := path
	if !filepath.IsAbs(path) && e.Root != "" {
		// Join cleans the path before the OS can resolve symlinks and '..'.
		separator := string(filepath.Separator)
		diskPath = strings.TrimSuffix(e.Root, separator) + separator + path
	}
	f, err := fileguard.OpenRegular(diskPath)
	if err != nil {
		return llm.TextAttachment{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxTextAttachmentBytes+1))
	if err != nil {
		return llm.TextAttachment{}, err
	}
	if len(data) > MaxTextAttachmentBytes {
		return llm.TextAttachment{}, fmt.Errorf("attachment %q exceeds the %d-byte limit", path, MaxTextAttachmentBytes)
	}
	if !utf8.Valid(data) {
		return llm.TextAttachment{}, fmt.Errorf("attachment %q must be a UTF-8 text file", path)
	}
	for _, b := range data {
		if b < 0x20 && b != '\t' && b != '\r' && b != '\n' && b != '\x1b' {
			return llm.TextAttachment{}, fmt.Errorf("attachment %q must be a UTF-8 text file", path)
		}
	}
	return llm.TextAttachment{Path: path, Content: string(data)}, nil
}
