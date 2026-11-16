//go:build !windows

package session

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"minimalharness/internal/llm"
)

func TestNonRegularImageBlobsDoNotBlockHydration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(imageBlobDir(), 0700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	if err := syscall.Mkfifo(filepath.Join(imageBlobDir(), digest), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	msgs := []llm.Message{{Role: "user", Images: []llm.Image{{Digest: digest}}}}
	go func() { HydrateImages(msgs); close(done) }()
	select {
	case <-done:
		if msgs[0].Images[0].Data != "" || msgs[0].Images[0].Unavailable == "" {
			t.Fatalf("FIFO accepted: %+v", msgs)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked image hydration")
	}
}
