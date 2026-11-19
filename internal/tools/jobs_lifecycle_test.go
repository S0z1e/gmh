package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCloseJobsStopsDirectAndRetainedCommands(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	e.SwitchSession("old")
	job, err := e.BashBackground(context.Background(), BashArgs{Command: "exec sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(job.Cancel)
	e.SwitchSession("new")
	direct := make(chan error, 1)
	go func() {
		_, err := e.Bash(context.Background(), BashArgs{Command: "printf started > direct-started; exec sleep 30"})
		direct <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(e.Root, "direct-started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("direct command did not start")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.CloseJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, done := job.Result(); !done {
		t.Fatal("retained session job still running")
	}
	select {
	case err := <-direct:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("direct command outcome=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("direct command survived shutdown")
	}
	if job.Delivered() {
		t.Fatal("shutdown acknowledged an unsaved result")
	}
	if _, err := e.Bash(context.Background(), BashArgs{Command: "printf forbidden > after-close"}); !errors.Is(err, ErrExecutorClosed) {
		t.Fatalf("direct launch after close: %v", err)
	}
	if _, err := e.BashBackground(context.Background(), BashArgs{Command: "printf forbidden > after-close"}); !errors.Is(err, ErrExecutorClosed) {
		t.Fatalf("background launch after close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "after-close")); !os.IsNotExist(err) {
		t.Fatalf("closed executor ran a command: %v", err)
	}
	if err := e.CloseJobs(ctx); err != nil {
		t.Fatal(err)
	}
}
