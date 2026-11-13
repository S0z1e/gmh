package tools

import (
	"testing"
	"time"
)

// Generous timeouts (Claude Code / OpenCode consensus): a normal
// install, build or test stays foreground instead of every real
// command becoming a background task at 10s.
func TestBashTimeoutDefaults(t *testing.T) {
	if DefaultBackgroundThreshold != 120*time.Second {
		t.Fatalf("DefaultBackgroundThreshold = %v, want 120s", DefaultBackgroundThreshold)
	}
	if DefaultBashTimeout != 120*time.Second {
		t.Fatalf("DefaultBashTimeout = %v, want 120s", DefaultBashTimeout)
	}
	if MaxBashTimeout != 600*time.Second {
		t.Fatalf("MaxBashTimeout = %v, want 600s", MaxBashTimeout)
	}
	e := &Executor{}
	if got := e.bashTimeout(BashArgs{Command: "echo x"}); got != 120*time.Second {
		t.Fatalf("bashTimeout default = %v, want 120s", got)
	}
	if got := e.bashTimeout(BashArgs{Command: "echo x", Timeout: 30}); got != 30*time.Second {
		t.Fatalf("bashTimeout per-call 30 = %v, want 30s", got)
	}
	if got := e.bashTimeout(BashArgs{Command: "echo x", Timeout: 900}); got != 600*time.Second {
		t.Fatalf("bashTimeout per-call 900 = %v, want 600s cap", got)
	}
	overridden := &Executor{BashTimeout: 45 * time.Second}
	if got := overridden.bashTimeout(BashArgs{Command: "echo x"}); got != 45*time.Second {
		t.Fatalf("bashTimeout executor override = %v, want 45s", got)
	}
}
