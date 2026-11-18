package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashOutputCapAfterShellExit(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	out, err := e.Bash(context.Background(), BashArgs{Command: `(sleep .5; dd if=/dev/zero bs=1048576 count=65 2>/dev/null; : > complete) & printf ready`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Output capture closed") {
		t.Fatalf("inherited output closure was not reported: %s", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(e.Root, "complete")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finite child did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := os.ReadDir(e.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		st, err := ent.Info()
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > OutputFileMaxBytes {
			t.Fatalf("output grew to %d bytes after shell exit; cap is %d", st.Size(), OutputFileMaxBytes)
		}
	}
}

func TestBashCaptureBurstCapsOnDisk(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	_, err := e.Bash(context.Background(), BashArgs{Command: `dd if=/dev/zero bs=1048576 count=65 2>/dev/null`})
	var capped *outputCapError
	if !errors.As(err, &capped) {
		t.Fatalf("65 MiB burst must return output cap error, got %v", err)
	}
	entries, err := os.ReadDir(e.OutputDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("output entries=%d err=%v", len(entries), err)
	}
	st, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != OutputFileMaxBytes {
		t.Fatalf("disk output=%d bytes, want exactly %d", st.Size(), OutputFileMaxBytes)
	}
}

func TestBashFailedShellReportsInheritedCaptureClosure(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	out, err := e.Bash(context.Background(), BashArgs{Command: `(sleep .5; printf late) & printf early; exit 2`})
	if err == nil {
		t.Fatal("failed shell returned success")
	}
	if !strings.Contains(out, "Output capture closed") {
		t.Fatalf("closure omitted from failed shell outcome: %s", out)
	}
}

func TestBashOutputCapKillsGroupAfterLeaderExit(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir()}
	command := `(dd if=/dev/zero bs=1048576 count=63 2>/dev/null; : > first-written; while ! test -f leader-exiting; do sleep .001; done; sleep .05; dd if=/dev/zero bs=1048576 count=2 2>/dev/null; : > survived-cap) & while ! test -f first-written; do sleep .001; done; : > leader-exiting; exit`
	_, err := e.Bash(context.Background(), BashArgs{Command: command})
	var capped *outputCapError
	if !errors.As(err, &capped) {
		t.Fatalf("expected cap after leader exit, got %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(e.Root, "survived-cap")); !os.IsNotExist(err) {
		t.Fatalf("child continued after output cap: %v", err)
	}
}
