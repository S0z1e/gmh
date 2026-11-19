package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionOutputQuotaResumeAndAdmission(t *testing.T) {
	ctx := context.Background()
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir(), OutputMaxBytes: 10}
	if _, err := e.Bash(ctx, BashArgs{Command: "printf 1234567"}); err != nil {
		t.Fatal(err)
	}
	job, err := e.BashBackground(ctx, BashArgs{Command: "printf abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	result, _ := job.Result()
	var quota *OutputQuotaError
	if !errors.As(result.Err, &quota) || !strings.Contains(result.Output, "3 bytes") || !strings.Contains(result.Preview.Notice, "10-byte limit") {
		t.Fatalf("quota result: %+v", result)
	}
	for _, exec := range []*Executor{e, {Root: e.Root, OutputDir: e.OutputDir, OutputMaxBytes: 10}} {
		if _, err := exec.Bash(ctx, BashArgs{Command: ": > forbidden"}); !errors.As(err, &quota) {
			t.Fatalf("full budget admitted a command: %v", err)
		}
		if _, err := exec.BashBackground(ctx, BashArgs{Command: ": > forbidden"}); !errors.As(err, &quota) {
			t.Fatalf("full budget admitted a job: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.Root, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("denied command executed: %v", err)
	}
	assertOutputBytes(t, e.OutputDir, 10)
}

func TestSessionOutputQuotaConcurrentCapture(t *testing.T) {
	const limit = 12000
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir(), OutputMaxBytes: limit}
	var jobs []*Job
	for range 8 {
		job, err := e.BashBackground(context.Background(), BashArgs{Command: "dd if=/dev/zero bs=4096 count=2 2>/dev/null"})
		var quota *OutputQuotaError
		if errors.As(err, &quota) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	for _, job := range jobs {
		<-job.Done()
	}
	assertOutputBytes(t, e.OutputDir, limit)
}

func assertOutputBytes(t *testing.T, dir string, want int64) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		st, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	if total != want {
		t.Fatalf("captured %d bytes, want %d", total, want)
	}
}

func TestSessionOutputQuotaDoesNotFallbackOnStorageFailure(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Root: root, OutputDir: filepath.Join(file, "output")}
	if _, err := e.Bash(context.Background(), BashArgs{Command: ": > forbidden"}); err == nil {
		t.Fatal("invalid storage bypassed admission")
	}
	if _, err := os.Stat(filepath.Join(root, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("command executed: %v", err)
	}
}

func TestSessionOutputQuotaLegacyReleasesStorage(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputMaxBytes: 3}
	for range 3 {
		out, err := e.Bash(context.Background(), BashArgs{Command: "printf abc"})
		if err != nil || out != "abc" {
			t.Fatalf("temporary budget not released: %q, %v", out, err)
		}
	}
}

func TestSessionOutputQuotaCountsOldFilesAndRejectsLinks(t *testing.T) {
	e := &Executor{Root: t.TempDir(), OutputDir: t.TempDir(), OutputMaxBytes: 5}
	if err := os.WriteFile(filepath.Join(e.OutputDir, "old.log"), []byte("123456"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := e.Bash(context.Background(), BashArgs{Command: ": > forbidden"})
	var quota *OutputQuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("old output ignored: %v", err)
	}
	if err := os.Remove(filepath.Join(e.OutputDir, "old.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.Root, "forbidden"), filepath.Join(e.OutputDir, "link.log")); err != nil {
		t.Skip(err)
	}
	if _, err := e.Bash(context.Background(), BashArgs{Command: ": > forbidden"}); err == nil {
		t.Fatal("accepted non-regular capture")
	}
}
