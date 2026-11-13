package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Write refuses an existing file by default, replaces it with
// overwrite:true, and writes a new file either way.
func TestWriteOverwrite(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	p := filepath.Join(root, "note.txt")

	out, err := e.Write(WriteArgs{Path: "note.txt", Content: "v1\n"})
	if err != nil || out != "wrote note.txt" {
		t.Fatalf("write new = (%q, %v), want (\"wrote note.txt\", nil)", out, err)
	}
	if _, err := e.Write(WriteArgs{Path: "note.txt", Content: "v2\n"}); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("write existing (default) error = %v, want a 'file exists' error", err)
	}
	out, err = e.Write(WriteArgs{Path: "note.txt", Content: "v2\n", Overwrite: true})
	if err != nil || out != "overwrote note.txt" {
		t.Fatalf("write overwrite = (%q, %v), want (\"overwrote note.txt\", nil)", out, err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "v2\n" {
		t.Fatalf("file after overwrite = %q (%v), want \"v2\\n\"", data, err)
	}
	out, err = e.Write(WriteArgs{Path: "fresh.txt", Content: "new\n", Overwrite: true})
	if err != nil || out != "wrote fresh.txt" {
		t.Fatalf("write new with overwrite = (%q, %v), want (\"wrote fresh.txt\", nil)", out, err)
	}
}
