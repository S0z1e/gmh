// Package turnbudget meters one turn independently of model stopping and task
// completion. It does not estimate missing provider usage or invent prices.
package turnbudget

import (
	"fmt"
	"math"
	"time"
)

// Policy uses nil for defaults and explicit zero to disable an individual cap.
// Token and dollar limits are opt-in: not all providers report billable usage.
type Policy struct {
	MaxDurationSeconds *int64   `json:"maxDurationSeconds,omitempty"`
	MaxToolCalls       *int64   `json:"maxToolCalls,omitempty"`
	MaxConcurrentJobs  *int64   `json:"maxConcurrentJobs,omitempty"`
	MaxTokens          *int64   `json:"maxTokens,omitempty"`
	MaxCostUSD         *float64 `json:"maxCostUSD,omitempty"`
}

type Limits struct {
	Duration       time.Duration `json:"duration_ns"`
	ToolCalls      int64         `json:"tool_calls"`
	ConcurrentJobs int64         `json:"concurrent_jobs"`
	Tokens         int64         `json:"tokens"`
	CostUSD        float64       `json:"cost_usd"`
}

func (p *Policy) Resolve() (Limits, error) {
	l := Limits{Duration: time.Hour, ToolCalls: 1000, ConcurrentJobs: 8}
	if p == nil {
		return l, nil
	}
	for _, v := range []*int64{p.MaxDurationSeconds, p.MaxToolCalls, p.MaxConcurrentJobs, p.MaxTokens} {
		if v != nil && *v < 0 {
			return Limits{}, fmt.Errorf("turn budget limits cannot be negative")
		}
	}
	if p.MaxDurationSeconds != nil {
		if *p.MaxDurationSeconds > math.MaxInt64/int64(time.Second) {
			return Limits{}, fmt.Errorf("turn duration overflows time.Duration")
		}
		l.Duration = time.Duration(*p.MaxDurationSeconds) * time.Second
	}
	if p.MaxToolCalls != nil {
		l.ToolCalls = *p.MaxToolCalls
	}
	if p.MaxConcurrentJobs != nil {
		l.ConcurrentJobs = *p.MaxConcurrentJobs
	}
	if p.MaxTokens != nil {
		l.Tokens = *p.MaxTokens
	}
	if p.MaxCostUSD != nil {
		if !finiteNonnegative(*p.MaxCostUSD) {
			return Limits{}, fmt.Errorf("turn dollar limit must be finite and nonnegative")
		}
		l.CostUSD = *p.MaxCostUSD
	}
	return l, nil
}

// Clone prevents a config merge or later caller edit from sharing limit pointers.
func (p *Policy) Clone() *Policy {
	if p == nil {
		return nil
	}
	q := *p
	cloneInt := func(v *int64) *int64 {
		if v == nil {
			return nil
		}
		n := *v
		return &n
	}
	q.MaxDurationSeconds = cloneInt(p.MaxDurationSeconds)
	q.MaxToolCalls = cloneInt(p.MaxToolCalls)
	q.MaxConcurrentJobs = cloneInt(p.MaxConcurrentJobs)
	q.MaxTokens = cloneInt(p.MaxTokens)
	if p.MaxCostUSD != nil {
		n := *p.MaxCostUSD
		q.MaxCostUSD = &n
	}
	return &q
}

// Exceeded is a resource stop, never a successful completion or user abort.
// Used may exceed Limit for usage delivered after an in-flight request ends.
type Exceeded struct {
	Resource string  `json:"resource"`
	Limit    float64 `json:"limit"`
	Used     float64 `json:"used"`
}

func (e *Exceeded) Error() string {
	return fmt.Sprintf("turn %s budget exhausted: used %g, limit %g", e.Resource, e.Used, e.Limit)
}

// Unavailable means a requested meter cannot be enforced from real evidence.
// It remains sticky for the turn: a later measured request cannot fill the gap.
type Unavailable struct {
	Resource string `json:"resource"`
	Reason   string `json:"reason"`
}

func (e *Unavailable) Error() string {
	return fmt.Sprintf("turn %s budget unavailable: %s", e.Resource, e.Reason)
}

// Usage distinguishes reported token counts and cost priced from configured
// rates from absent data. CostUSD must be nil when usage or required rates are
// unknown; a local token estimate is not reported usage.
type Usage struct {
	Tokens  *int64
	CostUSD *float64
}

type State struct {
	Started         time.Time `json:"started"`
	Limits          Limits    `json:"limits"`
	ToolCalls       int64     `json:"tool_calls"`
	ReportedTokens  int64     `json:"reported_tokens"`
	TokensComplete  bool      `json:"tokens_complete"`
	RecordedCostUSD float64   `json:"recorded_cost_usd"`
	CostComplete    bool      `json:"cost_complete"`
}

// Tracker is owned by the serial agent loop. Every provider attempt, including
// retries/compaction/grace, must report Usage once; tools are reserved per batch.
type Tracker struct {
	state       State
	unavailable *Unavailable
}

func New(policy *Policy, started time.Time, ratesAvailable bool) (*Tracker, error) {
	limits, err := policy.Resolve()
	if err != nil {
		return nil, err
	}
	if limits.CostUSD > 0 && !ratesAvailable {
		return nil, &Unavailable{Resource: "cost_usd", Reason: "no configured rates"}
	}
	return &Tracker{state: State{Started: started, Limits: limits, TokensComplete: true, CostComplete: true}}, nil
}
func (b *Tracker) State() State { return b.state }
func (b *Tracker) Deadline() (time.Time, bool) {
	if b.state.Limits.Duration == 0 {
		return time.Time{}, false
	}
	return b.state.Started.Add(b.state.Limits.Duration), true
}
func (b *Tracker) CheckDuration(now time.Time) error {
	if d, ok := b.Deadline(); ok && !now.Before(d) {
		return &Exceeded{Resource: "duration_seconds", Limit: b.state.Limits.Duration.Seconds(), Used: now.Sub(b.state.Started).Seconds()}
	}
	return nil
}

// CheckRequest permits the final response to use exactly the token/dollar cap,
// but never starts another provider attempt once that cap has been spent.
func (b *Tracker) CheckRequest(now time.Time) error {
	if err := b.CheckDuration(now); err != nil {
		return err
	}
	if b.unavailable != nil {
		return b.unavailable
	}
	l, s := b.state.Limits, b.state
	if l.Tokens > 0 && s.ReportedTokens >= l.Tokens {
		return &Exceeded{Resource: "tokens", Limit: float64(l.Tokens), Used: float64(s.ReportedTokens)}
	}
	if l.CostUSD > 0 && s.RecordedCostUSD >= l.CostUSD {
		return &Exceeded{Resource: "cost_usd", Limit: l.CostUSD, Used: s.RecordedCostUSD}
	}
	return nil
}

// ReserveTools is atomic for a provider batch. A rejected batch does not consume
// slots and must not execute a prefix. Counts include refused/invalid requests.
func (b *Tracker) ReserveTools(n int64) error {
	if n < 0 || n > math.MaxInt64-b.state.ToolCalls {
		return fmt.Errorf("invalid tool reservation")
	}
	next := b.state.ToolCalls + n
	if limit := b.state.Limits.ToolCalls; limit > 0 && next > limit {
		return &Exceeded{Resource: "tool_calls", Limit: float64(limit), Used: float64(next)}
	}
	b.state.ToolCalls = next
	return nil
}

// CheckJobLaunch uses an Executor-wide count, including retained sessions. It
// does not reserve IDs or expose another session's output. The loop serializes
// launches and obtains a fresh count before every potentially backgrounded call.
func (b *Tracker) CheckJobLaunch(running int64) error {
	if running < 0 || running == math.MaxInt64 {
		return fmt.Errorf("invalid running job count")
	}
	if limit := b.state.Limits.ConcurrentJobs; limit > 0 && running >= limit {
		return &Exceeded{Resource: "concurrent_jobs", Limit: float64(limit), Used: float64(running) + 1}
	}
	return nil
}

// RecordUsage retains real consumption even if it exceeds the cap. Missing
// counters remain unknown, not zero. Limits stop future requests; one already
// running request can exceed them before the provider reports its final usage.
func (b *Tracker) RecordUsage(u Usage) error {
	if u.Tokens == nil {
		b.state.TokensComplete = false
	} else if *u.Tokens < 0 || *u.Tokens > math.MaxInt64-b.state.ReportedTokens {
		b.state.TokensComplete = false
		b.unmetered("tokens", "invalid or overflowing usage")
	} else {
		b.state.ReportedTokens += *u.Tokens
	}
	if u.CostUSD == nil {
		b.state.CostComplete = false
	} else if !finiteNonnegative(*u.CostUSD) || !finiteNonnegative(b.state.RecordedCostUSD+*u.CostUSD) {
		b.state.CostComplete = false
		b.unmetered("cost_usd", "invalid or overflowing usage")
	} else {
		b.state.RecordedCostUSD += *u.CostUSD
	}
	l, s := b.state.Limits, b.state
	if l.Tokens > 0 && !s.TokensComplete {
		return b.unmetered("tokens", "provider usage is incomplete")
	}
	if l.CostUSD > 0 && !s.CostComplete {
		return b.unmetered("cost_usd", "usage or pricing evidence is incomplete")
	}
	if b.unavailable != nil {
		return b.unavailable
	}
	if l.Tokens > 0 && s.ReportedTokens > l.Tokens {
		return &Exceeded{Resource: "tokens", Limit: float64(l.Tokens), Used: float64(s.ReportedTokens)}
	}
	if l.CostUSD > 0 && s.RecordedCostUSD > l.CostUSD {
		return &Exceeded{Resource: "cost_usd", Limit: l.CostUSD, Used: s.RecordedCostUSD}
	}
	return nil
}
func (b *Tracker) unmetered(resource, reason string) error {
	if b.unavailable == nil {
		b.unavailable = &Unavailable{Resource: resource, Reason: reason}
	}
	return b.unavailable
}
func finiteNonnegative(n float64) bool { return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }
