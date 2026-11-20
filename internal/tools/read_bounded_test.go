package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadContextRejectsCanceledAndInvalidatesLargeSnapshot(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	path := filepath.Join(e.Root, "large")
	if err := os.WriteFile(path, []byte("small\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Read(ReadArgs{Path: "large"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxReadSnapshotBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := e.ReadContext(context.Background(), ReadArgs{Path: "large", Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if _, known := e.snapshots[e.snapshotPath("large")]; known {
		t.Fatal("large partial read authorized a snapshot")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.ReadContext(ctx, ReadArgs{Path: "large"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
}

func TestReadCancellationWhileWaitingForMutation(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	e.editMu.Lock()
	defer e.editMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.ReadContext(ctx, ReadArgs{Path: "."}); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancel lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read ignored cancellation while waiting for mutation lock")
	}
}

func TestReadLargeLineAndSkippedPrefixHaveBoundedScan(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	path := filepath.Join(e.Root, "large")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	part := strings.Repeat("x", 1<<20)
	for range 34 {
		if _, err := f.WriteString(part); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	out, err := e.ReadContext(context.Background(), ReadArgs{Path: "large", Limit: 1})
	if err != nil || !strings.Contains(out, "line truncated") || len(out) > MaxReadBytes+100 {
		t.Fatalf("giant line: bytes=%d, %v", len(out), err)
	}
	out, err = e.ReadContext(context.Background(), ReadArgs{Path: "large", Offset: 2})
	if err != nil || !strings.Contains(out, "scan limit") {
		t.Fatalf("skipped prefix: %q,%v", out, err)
	}
}

func TestReadDirectoryUsesBoundedSortedPages(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	for _, name := range []string{"z", "a", "d", "b", "c"} {
		if err := os.WriteFile(filepath.Join(e.Root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.ReadContext(context.Background(), ReadArgs{Path: ".", Offset: 2, Limit: 2})
	if err != nil || !strings.Contains(out, "b\nc\n... [truncated") || strings.Contains(out, "\nz") {
		t.Fatalf("directory page: %q,%v", out, err)
	}
	if _, err := e.Read(ReadArgs{Path: ".", Offset: maxDirectoryOffset + 2}); err == nil {
		t.Fatal("unbounded directory offset")
	}
}

func TestReadLargeDirectoryCapsRequestedPage(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	for i := 0; i < 1200; i++ {
		if err := os.WriteFile(filepath.Join(e.Root, fmt.Sprintf("entry-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := e.ReadContext(context.Background(), ReadArgs{Path: ".", Limit: 1000000})
	if err != nil || !strings.Contains(out, "entry-0000") || !strings.Contains(out, "entry-0499") || strings.Contains(out, "entry-0500") || !strings.Contains(out, "truncated") {
		t.Fatalf("unbounded directory page bytes=%d err=%v", len(out), err)
	}
}
