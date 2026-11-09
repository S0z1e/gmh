package tools

import (
	"context"
	"testing"
)

// Bash audits every execution with its exit code, and audits denylist
// denials instead of running.
func TestBashAudit(t *testing.T) {
	root := t.TempDir()
	var entries []AuditEntry
	e := &Executor{Root: root, Audit: func(en AuditEntry) { entries = append(entries, en) }}

	if _, err := e.Bash(context.Background(), BashArgs{Command: "echo ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bash(context.Background(), BashArgs{Command: "exit 3"}); err == nil {
		t.Fatal("exit 3 should error")
	}
	if _, err := e.Bash(context.Background(), BashArgs{Command: "rm -rf /"}); err == nil {
		t.Fatal("denylisted command should error")
	}
	if len(entries) != 3 {
		t.Fatalf("got %d audit entries, want 3", len(entries))
	}
	if entries[0].Kind != "bash" || entries[0].ExitCode != 0 {
		t.Fatalf("success entry = %+v", entries[0])
	}
	if entries[1].Kind != "bash" || entries[1].ExitCode != 3 {
		t.Fatalf("failure entry = %+v", entries[1])
	}
	if entries[2].Kind != "denied" || entries[2].ExitCode != -1 || entries[2].Error == "" {
		t.Fatalf("denial entry = %+v", entries[2])
	}
	if entries[2].Args != "rm -rf /" {
		t.Fatalf("denial args = %q", entries[2].Args)
	}
}

// The per-call timeout is honoured and audited as a timeout.
func TestBashTimeoutAudit(t *testing.T) {
	root := t.TempDir()
	var entries []AuditEntry
	e := &Executor{Root: root, Audit: func(en AuditEntry) { entries = append(entries, en) }}
	if _, err := e.Bash(context.Background(), BashArgs{Command: "sleep 5", Timeout: 1}); err == nil {
		t.Fatal("sleep past timeout should error")
	}
	if len(entries) != 1 || entries[0].ExitCode != -1 {
		t.Fatalf("timeout entry = %+v", entries)
	}
}
