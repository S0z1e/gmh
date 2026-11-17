package failureguard_test

import (
	"fmt"
	"testing"

	"minimalharness/internal/failureguard"
)

func failThree(g *failureguard.Guard, key string, kind failureguard.Kind, revision string) {
	for i := 0; i < failureguard.DenyAfter; i++ {
		g.ObserveFailure(key, kind, revision)
	}
}

func TestRepairProbeAndResolvedFailure(t *testing.T) {
	var g failureguard.Guard
	const key = "test command"
	if d := g.Admit(key, "initial"); d.Denied || d.Failures != 0 || d.StateProbe {
		t.Fatal(d)
	}
	failThree(&g, key, failureguard.VerificationFailure, "broken")
	if g.ObserveSuccess("read source") {
		t.Fatal("an unrelated read resolved the failed test")
	}
	if d := g.Admit(key, "broken"); !d.Denied || d.Failures != 3 {
		t.Fatalf("unchanged state admitted: %+v", d)
	}
	if d := g.Admit(key, "fixed"); d.Denied || !d.StateProbe || d.Kind != failureguard.VerificationFailure {
		t.Fatalf("repair cannot be checked: %+v", d)
	}
	if d := g.Admit(key, "fixed"); !d.Denied {
		t.Fatal("the same probe was admitted twice before its outcome")
	}
	if !g.ObserveSuccess(key) {
		t.Fatal("successful rerun did not resolve its failure")
	}
	if d := g.Admit(key, "fixed"); d.Denied || d.Failures != 0 || d.StateProbe {
		t.Fatalf("resolved failure still blocks work: %+v", d)
	}
}

func TestFailedAttemptOutputsAndOldVersionsCannotOpenProbe(t *testing.T) {
	var g failureguard.Guard
	const key = "failing build"
	for _, post := range []string{"log-1", "log-2", "log-3"} {
		g.ObserveFailure(key, failureguard.Environment, post)
	}
	for _, revision := range []string{"log-3", "log-2", "log-1", ""} {
		if d := g.Admit(key, revision); !d.Denied {
			t.Fatalf("failed output or unknown state opened retry: %q %+v", revision, d)
		}
	}
	if d := g.Admit(key, "edited-source"); d.Denied || !d.StateProbe {
		t.Fatal(d)
	}
	// The failed probe itself may produce another file version.
	g.ObserveFailure(key, failureguard.Environment, "edited-source-and-log")
	for _, revision := range []string{"edited-source", "edited-source-and-log", "log-3"} {
		if d := g.Admit(key, revision); !d.Denied {
			t.Fatalf("probe replay admitted: %q %+v", revision, d)
		}
	}
	if d := g.Admit(key, "second-edit"); d.Denied || !d.StateProbe {
		t.Fatal(d)
	}
	g.ObserveFailure(key, failureguard.Environment, "second-edit-and-log")
	if d := g.Admit(key, "third-edit"); !d.Denied {
		t.Fatal("per-call changed-state allowance was exceeded")
	}
}

func TestUnknownOrUnrepairableEvidenceFailsClosed(t *testing.T) {
	for _, kind := range []failureguard.Kind{failureguard.InvalidInput, failureguard.PolicyDenied, failureguard.Unknown, "", "unrecognized"} {
		var g failureguard.Guard
		failThree(&g, "call", kind, "before")
		if d := g.Admit("call", "after"); !d.Denied {
			t.Fatalf("workspace edit repaired %q: %+v", kind, d)
		}
	}
	for _, kind := range []failureguard.Kind{failureguard.Environment, failureguard.Transient, failureguard.VerificationFailure} {
		var g failureguard.Guard
		failThree(&g, "call", kind, "known")
		g.ObserveFailure("call", kind, "")
		if d := g.Admit("call", "new"); !d.Denied {
			t.Fatalf("unknown last failure reused stale state for %q: %+v", kind, d)
		}
		var known failureguard.Guard
		failThree(&known, "call", kind, "before")
		if d := known.Admit("call", "after"); d.Denied || !d.StateProbe {
			t.Fatalf("known repair rejected for %q: %+v", kind, d)
		}
	}
}

func TestTurnProbeAllowanceIsNotRefundedBySuccess(t *testing.T) {
	var g failureguard.Guard
	for i := 0; i < failureguard.MaxStateProbesPerTurn; i++ {
		key := fmt.Sprint(i)
		failThree(&g, key, failureguard.Environment, "before")
		if d := g.Admit(key, "after"); d.Denied || !d.StateProbe {
			t.Fatalf("probe %d rejected: %+v", i, d)
		}
		g.ObserveSuccess(key)
	}
	failThree(&g, "new fingerprint", failureguard.Environment, "before")
	if d := g.Admit("new fingerprint", "after"); !d.Denied {
		t.Fatal("switching fingerprint or succeeding refunded the turn allowance")
	}
	var nextTurn failureguard.Guard
	failThree(&nextTurn, "new fingerprint", failureguard.Environment, "before")
	if d := nextTurn.Admit("new fingerprint", "after"); d.Denied {
		t.Fatal("turn allowance leaked to another guard")
	}
}

func TestInFlightResultsCannotCauseForgottenVersionReplay(t *testing.T) {
	var g failureguard.Guard
	// Results already in flight can still arrive after the gate has closed.
	for i := 0; i < 100; i++ {
		g.ObserveFailure("job", failureguard.Environment, fmt.Sprint(i))
	}
	for _, revision := range []string{"0", "50", "99", "new edit"} {
		if d := g.Admit("job", revision); !d.Denied {
			t.Fatalf("bounded history reopened a forgotten version: %q %+v", revision, d)
		}
	}
}
