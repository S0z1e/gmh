//go:build !windows

package verification

import (
	"context"
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
