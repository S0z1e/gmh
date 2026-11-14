package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEditFile(t *testing.T, root, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func readEditFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEditLineRangeReplacesLines(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\ngamma\ndelta\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	if _, err := ex.Edit(EditArgs{Path: "x.txt", StartLine: 2, EndLine: 3, NewText: "BETA\nGAMMA"}); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, root, "x.txt"); got != "alpha\nBETA\nGAMMA\ndelta\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditLineRangeSingleLineDefaultsEnd(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\ngamma\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	// EndLine 0 means StartLine: one line.
	if _, err := ex.Edit(EditArgs{Path: "x.txt", StartLine: 2, NewText: "BETA"}); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, root, "x.txt"); got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditLineRangeIgnoresOldText(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	// OldText does not occur at all; the line form ignores it.
	if _, err := ex.Edit(EditArgs{Path: "x.txt", OldText: "zzz", StartLine: 1, EndLine: 1, NewText: "ALPHA"}); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, root, "x.txt"); got != "ALPHA\nbeta\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditLineRangeNoTrailingNewline(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta")
	ex.Read(ReadArgs{Path: "x.txt"})
	if _, err := ex.Edit(EditArgs{Path: "x.txt", StartLine: 2, NewText: "BETA"}); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, root, "x.txt"); got != "alpha\nBETA" {
		t.Fatalf("file = %q, want no trailing newline added", got)
	}
}

func TestEditLineRangeOutOfBounds(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	_, err := ex.Edit(EditArgs{Path: "x.txt", StartLine: 1, EndLine: 5, NewText: "x"})
	if err == nil || !strings.Contains(err.Error(), "beyond the last line (2)") {
		t.Fatalf("err = %v, want beyond-the-last-line naming 2 lines", err)
	}
	_, err = ex.Edit(EditArgs{Path: "x.txt", StartLine: 3, EndLine: 2, NewText: "x"})
	if err == nil || !strings.Contains(err.Error(), "before startLine") {
		t.Fatalf("err = %v, want reversed-range error", err)
	}
	_, err = ex.Edit(EditArgs{Path: "x.txt", StartLine: -1, NewText: "x"})
	if err == nil || !strings.Contains(err.Error(), "startLine must be >= 1") {
		t.Fatalf("err = %v, want startLine error", err)
	}
	if got := readEditFile(t, root, "x.txt"); got != "alpha\nbeta\n" {
		t.Fatalf("failed edits must not touch the file: %q", got)
	}
}

func TestEditNoMatchDiagnosticFirstLine(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "one\nfoo bar\nthree\n")
	_, err := ex.Edit(EditArgs{Path: "x.txt", OldText: "foo bar\nqux", NewText: "y"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "oldText must match exactly once, got 0 matches in x.txt") {
		t.Fatalf("prefix lost: %v", err)
	}
	if !strings.Contains(err.Error(), "line(s) 2") {
		t.Fatalf("want the first-line occurrence line: %v", err)
	}
}

func TestEditNoMatchDiagnosticClosestRegion(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\ngamma\ndelta\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	// First line "alfa" occurs nowhere; beta/gamma match contiguously.
	_, err := ex.Edit(EditArgs{Path: "x.txt", OldText: "alfa\nbeta\ngamma", NewText: "y"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "closest match") {
		t.Fatalf("want a closest-match region: %v", err)
	}
	if !strings.Contains(err.Error(), "2: beta") || !strings.Contains(err.Error(), "3: gamma") {
		t.Fatalf("want numbered region lines: %v", err)
	}
}

func TestEditNoMatchDiagnosticNoLineMatches(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	_, err := ex.Edit(EditArgs{Path: "x.txt", OldText: "zzz\nyyy", NewText: "y"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "no line of oldText matches") {
		t.Fatalf("want the no-match statement: %v", err)
	}
}

func TestEditMultiMatchDiagnosticListsLines(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "x\nx\nx\n")
	_, err := ex.Edit(EditArgs{Path: "x.txt", OldText: "x", NewText: "y"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "got 3 matches") {
		t.Fatalf("want 3-match prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "line(s) 1, 2, 3") {
		t.Fatalf("want all match lines listed: %v", err)
	}
}

func TestEditOldTextPathUnchanged(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	if _, err := ex.Edit(EditArgs{Path: "x.txt", OldText: "beta", NewText: "BETA"}); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, root, "x.txt"); got != "alpha\nBETA\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestDispatchLineEditCarriesDiffDetail(t *testing.T) {
	root := t.TempDir()
	ex := &Executor{Root: root}
	writeEditFile(t, root, "x.txt", "alpha\nbeta\ngamma\n")
	ex.Read(ReadArgs{Path: "x.txt"})
	res, err := ex.Dispatch(context.Background(), "edit", `{"path":"x.txt","startLine":2,"newText":"BETA"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "edited x.txt" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.Detail != "− beta\n+ BETA" {
		t.Fatalf("detail = %q, want the removed-line diff", res.Detail)
	}
}

func TestLineEditRejectsChangedFile(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	writeEditFile(t, root, "f", "a\nb\nc\n")
	if _, err := e.Read(ReadArgs{Path: "f"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "f"), []byte("new\na\nb\nc\n"), 0600)
	if _, err := e.Edit(EditArgs{Path: "f", StartLine: 2, NewText: "B"}); err == nil {
		t.Fatal("stale line edit accepted")
	}
	if got := readEditFile(t, root, "f"); got != "new\na\nb\nc\n" {
		t.Fatal(got)
	}
}
func TestLineEditRequiresRead(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	writeEditFile(t, root, "f", "a\nb\n")
	if _, err := e.Edit(EditArgs{Path: "f", StartLine: 2, NewText: "B"}); err == nil {
		t.Fatal("line edit without read accepted")
	}
}

func TestLineEditAfterOwnWrite(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	writeEditFile(t, root, "f", "old\n")
	if _, err := e.Write(WriteArgs{Path: "f", Content: "alpha\nbeta\n", Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Edit(EditArgs{Path: "f", StartLine: 2, OldText: "beta", NewText: "BETA"}); err != nil {
		t.Fatalf("line edit rejected the content just written by this executor: %v", err)
	}
	if got := readEditFile(t, root, "f"); got != "alpha\nBETA\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestLineEditAfterOwnWriteStillRejectsExternalChanges(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	if _, err := e.Write(WriteArgs{Path: "f", Content: "alpha\nbeta\n"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "f"), []byte("inserted\nalpha\nbeta\n"), 0600)
	if _, err := e.Edit(EditArgs{Path: "f", StartLine: 2, OldText: "beta", NewText: "BETA"}); err == nil || !strings.Contains(err.Error(), "file changed") {
		t.Fatalf("external change was accepted after own write: %v", err)
	}
	if got := readEditFile(t, root, "f"); got != "inserted\nalpha\nbeta\n" {
		t.Fatalf("external content changed: %q", got)
	}
}

func TestLineEditAfterOwnEdit(t *testing.T) {
	for _, lineForm := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "line"}[lineForm], func(t *testing.T) {
			root := t.TempDir()
			e := &Executor{Root: root}
			writeEditFile(t, root, "f", "alpha\nbeta\n")
			if _, err := e.Read(ReadArgs{Path: "f"}); err != nil {
				t.Fatal(err)
			}
			first := EditArgs{Path: "f", OldText: "alpha", NewText: "ALPHA"}
			if lineForm {
				first.StartLine = 1
			}
			if _, err := e.Edit(first); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Edit(EditArgs{Path: "f", StartLine: 2, NewText: "BETA"}); err != nil {
				t.Fatalf("own edit invalidated known content: %v", err)
			}
			if got := readEditFile(t, root, "f"); got != "ALPHA\nBETA\n" {
				t.Fatalf("file = %q", got)
			}
		})
	}
}

func TestFailedWritePreservesReadVersion(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	writeEditFile(t, root, "f", "alpha\nbeta\n")
	if _, err := e.Read(ReadArgs{Path: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write(WriteArgs{Path: "f", Content: "rejected"}); err == nil {
		t.Fatal("write unexpectedly overwrote existing file")
	}
	if _, err := e.Edit(EditArgs{Path: "f", StartLine: 2, NewText: "BETA"}); err != nil {
		t.Fatalf("failed write invalidated unchanged content: %v", err)
	}
}

func TestTextEditAfterExternalChangeDoesNotTrustLineNumbers(t *testing.T) {
	for _, baseline := range []string{"read", "write"} {
		t.Run(baseline, func(t *testing.T) {
			root := t.TempDir()
			e := &Executor{Root: root}
			original := "alpha\nbeta\ngamma\n"
			if baseline == "write" {
				if _, err := e.Write(WriteArgs{Path: "f", Content: original}); err != nil {
					t.Fatal(err)
				}
			} else {
				writeEditFile(t, root, "f", original)
				if _, err := e.Read(ReadArgs{Path: "f"}); err != nil {
					t.Fatal(err)
				}
			}
			writeEditFile(t, root, "f", "inserted\n"+original)
			if _, err := e.Edit(EditArgs{Path: "f", OldText: "gamma", NewText: "GAMMA"}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Edit(EditArgs{Path: "f", StartLine: 2, OldText: "beta", NewText: "BETA"}); err == nil {
				t.Fatal("text edit made stale line numbers trusted")
			}
			if got := readEditFile(t, root, "f"); got != "inserted\nalpha\nbeta\nGAMMA\n" {
				t.Fatalf("stale line edit changed the file: %q", got)
			}
			if _, err := e.Read(ReadArgs{Path: "f"}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Edit(EditArgs{Path: "f", StartLine: 3, NewText: "BETA"}); err != nil {
				t.Fatalf("fresh read did not restore line editing: %v", err)
			}
			if got := readEditFile(t, root, "f"); got != "inserted\nalpha\nBETA\nGAMMA\n" {
				t.Fatalf("file after fresh read/edit = %q", got)
			}
		})
	}
}

func TestTextEditsWithoutKnownVersionDoNotEnableLineEdit(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	writeEditFile(t, root, "f", "alpha\nbeta\ngamma\n")
	for _, text := range []string{"alpha", "beta"} {
		if _, err := e.Edit(EditArgs{Path: "f", OldText: text, NewText: strings.ToUpper(text)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Edit(EditArgs{Path: "f", StartLine: 3, NewText: "GAMMA"}); err == nil {
		t.Fatal("text edits established a version of unread surrounding content")
	}
	if got := readEditFile(t, root, "f"); got != "ALPHA\nBETA\ngamma\n" {
		t.Fatalf("unread line edit changed the file: %q", got)
	}
}
