package tools

import (
	"context"
	"errors"
	"testing"
)

func TestCloseJobsWaitsForRejectedBashAudit(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "registered"}[registered], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			e := &Executor{Root: t.TempDir(), Audit: func(AuditEntry) { close(entered); <-release }}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if registered {
					_, _ = e.BashBackground(context.Background(), BashArgs{Command: "rm -rf /"})
				} else {
					_, _ = e.Bash(context.Background(), BashArgs{Command: "rm -rf /"})
				}
			}()
			<-entered
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := e.CloseJobs(ctx)
			close(release)
			<-done
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("CloseJobs reported success with rejected Bash audit pending: %v", err)
			}
		})
	}
}
