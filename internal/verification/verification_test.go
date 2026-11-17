package verification

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeInput(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func capture(t *testing.T, root string, exclude ...string) Snapshot {
	t.Helper()
	state, err := Capture(context.Background(), root, exclude)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version == "" {
		t.Fatal("empty version")
	}
	return state
}

func TestVersionTracksBytesNamesModesDirectoriesAndWorkspace(t *testing.T) {
	root := t.TempDir()
	writeInput(t, root, "code.go", "first")
	first := capture(t, root)
	if got := capture(t, root); got.Version != first.Version {
		t.Fatal("unchanged inputs changed version")
	}
	info, err := os.Stat(filepath.Join(root, "code.go"))
	if err != nil {
		t.Fatal(err)
	}
	writeInput(t, root, "code.go", "other") // same size; restore mtime to rule out stat-only versions
	if err := os.Chtimes(filepath.Join(root, "code.go"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second := capture(t, root)
	if first.Version == second.Version {
		t.Fatal("changed bytes with same size/mtime reused version")
	}
	writeInput(t, root, "code.go", "first")
	if capture(t, root).Version != first.Version {
		t.Fatal("mtime-only change invalidated identical content")
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	withDir := capture(t, root)
	if withDir.Version == first.Version {
		t.Fatal("empty directory missing from version")
	}
	if err := os.Rename(filepath.Join(root, "code.go"), filepath.Join(root, "renamed.go")); err != nil {
		t.Fatal(err)
	}
	renamed := capture(t, root)
	if renamed.Version == withDir.Version {
		t.Fatal("filename missing from version")
	}
	otherRoot := t.TempDir()
	writeInput(t, otherRoot, "code.go", "first")
	if capture(t, otherRoot).Version == first.Version {
		t.Fatal("different workspace reused version")
	}
}

func TestExclusionsAreExplicitAndPartOfVersion(t *testing.T) {
	root := t.TempDir()
	writeInput(t, root, "code.go", "source")
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "outputs"), 0700); err != nil {
		t.Fatal(err)
	}
	writeInput(t, root, ".git/HEAD", "old")
	writeInput(t, root, "outputs/result", "one")
	first := capture(t, root, "outputs")
	writeInput(t, root, ".git/HEAD", "new")
	writeInput(t, root, "outputs/result", "two")
	if capture(t, root, "outputs").Version != first.Version {
		t.Fatal("excluded output affected version")
	}
	all := capture(t, root)
	if all.Version == first.Version {
		t.Fatal("exclusion policy missing from version")
	}
	for _, bad := range []string{"", ".", "..", "../outside", root} {
		got, err := Capture(context.Background(), root, []string{bad})
		if err == nil || got.Version != "" {
			t.Fatalf("invalid exclusion %q returned version", bad)
		}
	}
}

func TestCaptureRejectsUnsupportedAndOversizedInputs(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeInput(t, outside, "secret", "not part of workspace")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err == nil {
		got, err := Capture(context.Background(), root, nil)
		if err == nil || got.Version != "" {
			t.Fatal("symlink produced complete version")
		}
		capture(t, root, "link")
		if err := os.Remove(filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	large, err := os.Create(filepath.Join(root, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	large.Close()
	if got, err := Capture(context.Background(), root, nil); err == nil || got.Version != "" {
		t.Fatal("oversized input produced a version")
	}
	capture(t, root, "large")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := Capture(ctx, root, []string{"large"}); err == nil || got.Version != "" {
		t.Fatal("cancelled capture produced a version")
	}
}

func TestEvidenceRequiresExactSuccessfulCurrentAttempt(t *testing.T) {
	root := t.TempDir()
	writeInput(t, root, "code.go", "old")
	state := capture(t, root)
	check := Check{Name: "tests", Command: "go test ./..."}
	dir, err := check.Directory(state.Root.Path)
	if err != nil {
		t.Fatal(err)
	}
	zero, one := 0, 1
	now := time.Now().UTC()
	good := Evidence{Check: check.Name, Command: check.Command, Cwd: dir, Version: state.Version, ExitCode: &zero, Started: now, Finished: now.Add(time.Second)}
	if !good.Current(check, state) {
		t.Fatal("current pass rejected")
	}
	for _, change := range []func(*Evidence){
		func(e *Evidence) { e.Command = "true" }, func(e *Evidence) { e.Check = "other" },
		func(e *Evidence) { e.Cwd = t.TempDir() }, func(e *Evidence) { e.Version = "old" },
		func(e *Evidence) { e.ExitCode = nil }, func(e *Evidence) { e.ExitCode = &one },
		func(e *Evidence) { e.Error = "interrupted" }, func(e *Evidence) { e.Started = time.Time{} },
		func(e *Evidence) { e.Finished = e.Started.Add(-time.Second) },
	} {
		bad := good
		change(&bad)
		if bad.Current(check, state) {
			t.Fatalf("bad evidence accepted: %+v", bad)
		}
	}
	writeInput(t, root, "code.go", "new")
	if good.Current(check, capture(t, root)) {
		t.Fatal("old pass accepted after an edit")
	}
	for _, bad := range []Check{{Name: "", Command: "true"}, {Name: "x", Command: " "}, {Name: "x", Command: "true", Cwd: t.TempDir()}, {Name: "x", Command: "true", Cwd: "code.go"}} {
		if _, err := bad.Directory(state.Root.Path); err == nil {
			t.Fatalf("invalid check accepted: %+v", bad)
		}
	}
	if got, err := Capture(context.Background(), root, []string{strings.Repeat("a", 100)}); err != nil || got.Version == "" {
		t.Fatalf("nonexistent relative exclusion: %v", err)
	}
}
