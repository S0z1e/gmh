//go:build !windows

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

func fakeGrepProducer(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestGrepProducerFailuresKeepPartialResults(t *testing.T) {
	const match = `{"type":"match","data":{"path":{"text":"hit.txt"},"line_number":1,"lines":{"text":"MATCH\n"}}}`
	for _, ending := range []string{"exit 2", "printf '%s\\n' 'BROKEN'; exec sleep 30"} {
		t.Run(ending, func(t *testing.T) {
			fakeGrepProducer(t, "printf '%s\\n' '"+match+"'\n"+ending+"\n")
			e := &Executor{Root: t.TempDir()}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r, err := e.grepRg(ctx, e.Root, GrepArgs{Pattern: "MATCH"}, 10, 0)
			if err == nil || errors.Is(err, context.DeadlineExceeded) || r.Count != 1 || !r.Truncated || !r.CountLowerBound || !strings.Contains(r.Text, "hit.txt:1: MATCH") || strings.Contains(r.Text, "no matches") {
				t.Fatalf("partial failure discarded or accepted: %+v err=%v", r, err)
			}
		})
	}
}

func TestGrepCancellationStopsProducerAfterOutput(t *testing.T) {
	binary := fakeGrepProducer(t, "printf 'first\\n'\nexec sleep 30\n")
	e := &Executor{Root: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := 0
	_, err := e.streamGrep(ctx, binary, nil, func(line string) error {
		if line != "first" {
			t.Errorf("line = %q", line)
		}
		lines++
		cancel()
		return nil
	})
	if lines != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("lines=%d error=%v", lines, err)
	}
}
