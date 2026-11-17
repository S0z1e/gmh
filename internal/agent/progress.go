package agent

import (
	"context"
	"errors"
	"net"
	"time"

	"minimalharness/internal/failureguard"
	"minimalharness/internal/tools"
	"minimalharness/internal/verification"
)

const maxProgressCaptures = 64

type turnProgress struct {
	ctx        context.Context
	guard      failureguard.Guard
	failures   int
	baseline   string
	captures   int
	lastErrors []string
	jobs       map[string]bool
}

// Capture only while handling failures or checking a possible repair. Ordinary
// successful turns pay no snapshot cost. Each capture is independently bounded,
// and exhaustion/unsupported inputs remain unknown rather than implying change.
func (c *Config) progressRevision() string {
	p := c.progress
	if p == nil || c.Exec == nil || p.ctx.Err() != nil || p.captures >= maxProgressCaptures || c.Exec.HasRunningWorkspaceJobs() {
		return ""
	}
	p.captures++
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	var exclude []string
	if c.Verification != nil {
		exclude = c.Verification.Exclude
	}
	state, err := verification.Capture(ctx, c.Exec.Root, exclude)
	if err != nil || c.Exec.HasRunningWorkspaceJobs() {
		return ""
	}
	return state.Version
}

func (c *Config) progressAdmit(key string) failureguard.Decision {
	d := c.progress.guard.Admit(key, "")
	if d.Denied {
		d = c.progress.guard.Admit(key, c.progressRevision())
		if d.StateProbe {
			c.progress.failures = 0
		}
	}
	return d
}

func (c *Config) progressFailure(key string, kind failureguard.Kind) int {
	p := c.progress
	p.failures++
	p.baseline = c.progressRevision()
	return p.guard.ObserveFailure(key, kind, p.baseline)
}

func (c *Config) progressSuccess(key, tool string) {
	p := c.progress
	if p.guard.ObserveSuccess(key) {
		p.failures = 0
	}
	if p.failures == 0 {
		return
	}
	switch tool {
	case "read", "glob", "grep", "load_skill", "webfetch", "ask_user":
		return
	}
	after := c.progressRevision()
	if after != "" && p.baseline != "" && after != p.baseline {
		p.failures = 0
		p.baseline = after
	}
}

func failureKind(err error, verificationCheck bool) failureguard.Kind {
	var policy *tools.PolicyError
	if errors.As(err, &policy) {
		return failureguard.PolicyDenied
	}
	var network net.Error
	if tools.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return failureguard.Transient
	}
	if verificationCheck {
		return failureguard.VerificationFailure
	}
	return failureguard.Environment
}

// Every collection/drain path uses this seam. Accounting is once per job per
// turn, independent of which path delivered it or whether persistence failed.
func (c *Config) progressJob(job *tools.Job, res tools.JobResult, obs string) string {
	p := c.progress
	if p == nil || p.jobs[job.ID] || p.ctx.Err() != nil || c.budgetError() != nil {
		return obs
	}
	p.jobs[job.ID] = true
	key := job.FailureKey()
	if key == "" {
		// Programmatic jobs may have no canonical agent invocation. Never merge
		// unrelated working directories merely because their commands match.
		key = "background_job:" + job.ID
	}
	if res.Err != nil {
		n := c.progressFailure(key, failureKind(res.Err, false))
		if n >= FailureWarnAfter {
			obs += "\n" + FailureNote("bash", n)
		}
		recordLastError(&p.lastErrors, obs)
	} else {
		c.progressSuccess(key, "bash")
	}
	return obs
}
