package tools

import (
	"context"
	"testing"
	"time"
)

func TestInvocationIdentityReachesBashAndBackgroundAudit(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			audits := make(chan AuditEntry, 1)
			e := &Executor{Root: t.TempDir(), Audit: func(entry AuditEntry) { audits <- entry }}
			ctx, cancel := context.WithCancel(WithInvocation(context.Background(), "invocation", "call"))
			defer cancel()
			if background {
				job, err := e.BashBackground(ctx, BashArgs{Command: "printf done", Background: true})
				if err != nil {
					t.Fatal(err)
				}
				defer job.Cancel()
				if job.InvocationID != "invocation" || job.ToolCallID != "call" {
					t.Fatalf("job lost identity: %+v", job)
				}
				cancel()
			} else if _, err := e.Bash(ctx, BashArgs{Command: "printf done"}); err != nil {
				t.Fatal(err)
			}
			select {
			case entry := <-audits:
				if entry.InvocationID != "invocation" || entry.ToolCallID != "call" || entry.ExitCode != 0 {
					t.Fatalf("audit lost identity or cancellation ownership: %+v", entry)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("bash audit not delivered")
			}
		})
	}
}
