//go:build !windows

package verification

import (
	"context"
	"minimalharness/internal/workspace"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCaptureNeverReadsFIFOAndTracksPermissions(t *testing.T) {
	root := t.TempDir()
	writeInput(t, root, "code", "source")
	first := capture(t, root)
	if err := os.Chmod(filepath.Join(root, "code"), 0700); err != nil {
		t.Fatal(err)
	}
	if capture(t, root).Version == first.Version {
		t.Fatal("execute permission missing from version")
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	if got, err := Capture(context.Background(), root, nil); err == nil || got.Version != "" {
		t.Fatal("FIFO produced a complete version")
	}
	capture(t, root, "pipe")
}

func TestVersionPreservesRawFilenameBytes(t *testing.T) {
	root := t.TempDir()
	first := "name" + string([]byte{0xff})
	second := "name" + string([]byte{0xfe})
	if err := os.WriteFile(filepath.Join(root, first), []byte("same"), 0600); err != nil {
		t.Skipf("non-UTF8 filenames unsupported: %v", err)
	}
	before := capture(t, root)
	if err := os.Rename(filepath.Join(root, first), filepath.Join(root, second)); err != nil {
		t.Skipf("non-UTF8 rename unsupported: %v", err)
	}
	if capture(t, root).Version == before.Version {
		t.Fatal("different raw filename bytes collided")
	}
}

func TestSnapshotRootFIFOReplacementIsRejectedWithoutBlocking(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := workspace.Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(root, 0600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	if opened, err := openSnapshotRoot(identity); err == nil {
		opened.Close()
		t.Fatal("FIFO opened as workspace")
	}
}
