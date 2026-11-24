// Package turnbudget meters one turn independently of model stopping and task
// completion. It does not estimate missing provider usage or invent prices.
package turnbudget

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"
)

// Default budget limits applied when a Policy omits the corresponding field.
// Explicit zero disables an individual cap.
const (
	DefaultMaxDuration       = time.Hour
	DefaultMaxToolCalls      = int64(1000)
	DefaultMaxConcurrentJobs = int64(8)
	DefaultMaxTokens         = int64(2_000_000)
	DefaultMaxSessionTokens  = int64(10_000_000)
)

// Policy uses nil for defaults and explicit zero to disable an individual cap.
// MaxSessionTokens and MaxSessionCostUSD enforce cumulative limits persisted
// across turns and --resume within the same session. StrictMetering opts into
// fail-closed enforcement when historical or provider metering has gaps.
type Policy struct {
	MaxDurationSeconds *int64   `json:"maxDurationSeconds,omitempty"`
	MaxToolCalls       *int64   `json:"maxToolCalls,omitempty"`
	MaxConcurrentJobs  *int64   `json:"maxConcurrentJobs,omitempty"`
	MaxTokens          *int64   `json:"maxTokens,omitempty"`
	MaxCostUSD         *float64 `json:"maxCostUSD,omitempty"`
	MaxSessionTokens   *int64   `json:"maxSessionTokens,omitempty"`
	MaxSessionCostUSD  *float64 `json:"maxSessionCostUSD,omitempty"`
	StrictMetering     *bool    `json:"strictMetering,omitempty"`
}

type Limits struct {
	Duration       time.Duration `json:"duration_ns"`
	ToolCalls      int64         `json:"tool_calls"`
	ConcurrentJobs int64         `json:"concurrent_jobs"`
	Tokens         int64         `json:"tokens"`
	CostUSD        float64       `json:"cost_usd"`
	SessionTokens  int64         `json:"session_tokens,omitempty"`
	SessionCostUSD float64       `json:"session_cost_usd,omitempty"`
	StrictMetering bool          `json:"strict_metering,omitempty"`
}

func (p *Policy) Resolve() (Limits, error) {
	l := Limits{
		Duration:       DefaultMaxDuration,
		ToolCalls:      DefaultMaxToolCalls,
		ConcurrentJobs: DefaultMaxConcurrentJobs,
		Tokens:         DefaultMaxTokens,
		SessionTokens:  DefaultMaxSessionTokens,
	}
	if p == nil {
		return l, nil
	}
	for _, v := range []*int64{p.MaxDurationSeconds, p.MaxToolCalls, p.MaxConcurrentJobs, p.MaxTokens, p.MaxSessionTokens} {
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
	if p.MaxSessionTokens != nil {
		l.SessionTokens = *p.MaxSessionTokens
	}
	if p.MaxCostUSD != nil {
		if !finiteNonnegative(*p.MaxCostUSD) {
			return Limits{}, fmt.Errorf("turn dollar limit must be finite and nonnegative")
		}
		l.CostUSD = *p.MaxCostUSD
	}
	if p.MaxSessionCostUSD != nil {
		if !finiteNonnegative(*p.MaxSessionCostUSD) {
			return Limits{}, fmt.Errorf("session dollar limit must be finite and nonnegative")
		}
		l.SessionCostUSD = *p.MaxSessionCostUSD
	}
	if p.StrictMetering != nil {
		l.StrictMetering = *p.StrictMetering
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
	cloneFloat := func(v *float64) *float64 {
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
	q.MaxSessionTokens = cloneInt(p.MaxSessionTokens)
	q.MaxCostUSD = cloneFloat(p.MaxCostUSD)
	q.MaxSessionCostUSD = cloneFloat(p.MaxSessionCostUSD)
	if p.StrictMetering != nil {
		b := *p.StrictMetering
		q.StrictMetering = &b
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
// rates from absent data. Both cost fields must be nil when usage or required
// rates are unknown; a local token estimate is not reported usage.
type Usage struct {
	Tokens  *int64
	CostUSD *float64
	// ExactCostUSD accepts a decimal-priced amount without a float conversion.
	// Supply at most one cost representation. RecordUsage copies the value.
	ExactCostUSD *big.Rat
	// TokensPartial retains a reported subtotal without claiming the entire
	// attempt was measured (for example, a stream lost its output counter).
	TokensPartial bool
}

type State struct {
	Started                time.Time `json:"started"`
	Limits                 Limits    `json:"limits"`
	ToolCalls              int64     `json:"tool_calls"`
	ReportedTokens         int64     `json:"reported_tokens"`
	TokensComplete         bool      `json:"tokens_complete"`
	RecordedCostUSD        float64   `json:"recorded_cost_usd"`
	CostComplete           bool      `json:"cost_complete"`
	SessionReportedTokens  int64     `json:"session_reported_tokens,omitempty"`
	SessionTokensComplete  bool      `json:"session_tokens_complete,omitempty"`
	SessionRecordedCostUSD float64   `json:"session_recorded_cost_usd,omitempty"`
	SessionCostComplete    bool      `json:"session_cost_complete,omitempty"`
	BaselineIncomplete     bool      `json:"baseline_incomplete,omitempty"`
	UnmeteredAttempts      int       `json:"unmetered_attempts,omitempty"`
}

// Tracker is owned by the serial agent loop. Every provider attempt, including
// retries/compaction/grace, must report Usage once; tools are reserved per batch.
type Tracker struct {
	state       State
	unavailable *Unavailable
	// Interpret each finite float's shortest round-trip decimal as its USD
	// amount. Accumulate and compare those decimals exactly, without rounding
	// each request to a fixed unit (which could erase tiny billable amounts).
	cost             big.Rat
	costLimit        big.Rat
	sessionCost      big.Rat
	sessionCostLimit big.Rat
}

func New(policy *Policy, started time.Time, ratesAvailable bool) (*Tracker, error) {
	limits, err := policy.Resolve()
	if err != nil {
		return nil, err
	}
	costComplete := true
	baselineIncomplete := false
	if (limits.CostUSD > 0 || limits.SessionCostUSD > 0) && !ratesAvailable {
		if limits.StrictMetering {
			resource := "cost_usd"
			if limits.CostUSD == 0 {
				resource = "session_cost_usd"
			}
			return nil, &Unavailable{Resource: resource, Reason: "no configured rates"}
		}
		costComplete = false
		baselineIncomplete = true
	}
	b := &Tracker{state: State{Started: started, Limits: limits, TokensComplete: true, CostComplete: costComplete, SessionTokensComplete: true, SessionCostComplete: costComplete, BaselineIncomplete: baselineIncomplete}}
	b.costLimit.Set(decimal(limits.CostUSD))
	b.sessionCostLimit.Set(decimal(limits.SessionCostUSD))
	return b, nil
}

// SeedSessionUsage initializes cumulative session counters from the persisted
// usage journal so session-level caps survive across turns and --resume.
func (b *Tracker) SeedSessionUsage(tokens int64, costUSD *big.Rat, tokensComplete, costComplete bool) error {
	amount, _ := costUSD.Float64()
	if tokens < 0 || costUSD.Sign() < 0 || !finiteNonnegative(amount) {
		b.state.SessionTokensComplete = false
		b.state.SessionCostComplete = false
		if b.state.Limits.SessionTokens > 0 {
			return b.unmetered("session_tokens", "invalid persisted session usage")
		}
		if b.state.Limits.SessionCostUSD > 0 {
			return b.unmetered("session_cost_usd", "invalid persisted session cost")
		}
		return nil
	}
	b.state.SessionReportedTokens = tokens
	b.state.SessionTokensComplete = tokensComplete
	b.sessionCost.Set(costUSD)
	b.state.SessionRecordedCostUSD = amount
	b.state.SessionCostComplete = b.state.SessionCostComplete && costComplete
	if (b.state.Limits.SessionTokens > 0 && !tokensComplete) || (b.state.Limits.SessionCostUSD > 0 && !b.state.SessionCostComplete) {
		b.state.BaselineIncomplete = true
	}
	if b.state.Limits.StrictMetering {
		if b.state.Limits.SessionTokens > 0 && !tokensComplete {
			return b.unmetered("session_tokens", "persisted session usage is incomplete")
		}
		if b.state.Limits.SessionCostUSD > 0 && !b.state.SessionCostComplete {
			return b.unmetered("session_cost_usd", "persisted session cost is incomplete")
		}
	}
	return nil
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
	if l.SessionTokens > 0 && s.SessionReportedTokens >= l.SessionTokens {
		return &Exceeded{Resource: "session_tokens", Limit: float64(l.SessionTokens), Used: float64(s.SessionReportedTokens)}
	}
	if l.CostUSD > 0 && b.cost.Cmp(&b.costLimit) >= 0 {
		return &Exceeded{Resource: "cost_usd", Limit: l.CostUSD, Used: s.RecordedCostUSD}
	}
	if l.SessionCostUSD > 0 && b.sessionCost.Cmp(&b.sessionCostLimit) >= 0 {
		return &Exceeded{Resource: "session_cost_usd", Limit: l.SessionCostUSD, Used: s.SessionRecordedCostUSD}
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
	missingTokens := false
	missingCost := false
	if u.Tokens == nil {
		b.state.TokensComplete = false
		b.state.SessionTokensComplete = false
		missingTokens = true
	} else if *u.Tokens < 0 || *u.Tokens > math.MaxInt64-b.state.ReportedTokens {
		b.state.TokensComplete = false
		b.state.SessionTokensComplete = false
		b.unmetered("tokens", "invalid or overflowing usage")
	} else {
		b.state.ReportedTokens += *u.Tokens
		if *u.Tokens > math.MaxInt64-b.state.SessionReportedTokens {
			b.state.SessionTokensComplete = false
			b.unmetered("session_tokens", "invalid or overflowing session usage")
		} else {
			b.state.SessionReportedTokens += *u.Tokens
		}
	}
	if u.TokensPartial {
		b.state.TokensComplete = false
		b.state.SessionTokensComplete = false
		missingTokens = true
	}
	var cost *big.Rat
	invalidCost := false
	if u.ExactCostUSD != nil {
		invalidCost = u.CostUSD != nil || u.ExactCostUSD.Sign() < 0
		if !invalidCost {
			cost = u.ExactCostUSD
		}
	} else if u.CostUSD != nil {
		invalidCost = !finiteNonnegative(*u.CostUSD)
		if !invalidCost {
			cost = decimal(*u.CostUSD)
		}
	}
	if invalidCost {
		b.state.CostComplete = false
		b.state.SessionCostComplete = false
		b.unmetered("cost_usd", "invalid or ambiguous usage")
	} else if cost == nil {
		b.state.CostComplete = false
		b.state.SessionCostComplete = false
		missingCost = true
	} else {
		next := new(big.Rat).Add(&b.cost, cost)
		amount, _ := next.Float64()
		if !finiteNonnegative(amount) {
			b.state.CostComplete = false
			b.unmetered("cost_usd", "invalid or overflowing usage")
		} else {
			b.cost.Set(next)
			// State is a float for reporting; only the exact decimal sum
			// above is used for enforcement, even below float resolution.
			b.state.RecordedCostUSD = amount
		}
		nextSession := new(big.Rat).Add(&b.sessionCost, cost)
		sessionAmount, _ := nextSession.Float64()
		if !finiteNonnegative(sessionAmount) {
			b.state.SessionCostComplete = false
			b.unmetered("session_cost_usd", "invalid or overflowing session usage")
		} else {
			b.sessionCost.Set(nextSession)
			b.state.SessionRecordedCostUSD = sessionAmount
		}
	}
	if missingTokens || missingCost {
		b.state.UnmeteredAttempts++
	}
	l, s := b.state.Limits, b.state
	if l.StrictMetering {
		if l.Tokens > 0 && !s.TokensComplete {
			return b.unmetered("tokens", "provider usage is incomplete")
		}
		if l.SessionTokens > 0 && !s.SessionTokensComplete {
			return b.unmetered("session_tokens", "provider usage is incomplete")
		}
		if l.CostUSD > 0 && !s.CostComplete {
			return b.unmetered("cost_usd", "usage or pricing evidence is incomplete")
		}
		if l.SessionCostUSD > 0 && !s.SessionCostComplete {
			return b.unmetered("session_cost_usd", "usage or pricing evidence is incomplete")
		}
	}
	if b.unavailable != nil {
		return b.unavailable
	}
	if l.Tokens > 0 && s.ReportedTokens > l.Tokens {
		return &Exceeded{Resource: "tokens", Limit: float64(l.Tokens), Used: float64(s.ReportedTokens)}
	}
	if l.SessionTokens > 0 && s.SessionReportedTokens > l.SessionTokens {
		return &Exceeded{Resource: "session_tokens", Limit: float64(l.SessionTokens), Used: float64(s.SessionReportedTokens)}
	}
	if l.CostUSD > 0 && b.cost.Cmp(&b.costLimit) > 0 {
		return &Exceeded{Resource: "cost_usd", Limit: l.CostUSD, Used: s.RecordedCostUSD}
	}
	if l.SessionCostUSD > 0 && b.sessionCost.Cmp(&b.sessionCostLimit) > 0 {
		return &Exceeded{Resource: "session_cost_usd", Limit: l.SessionCostUSD, Used: s.SessionRecordedCostUSD}
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

// decimal accepts only validated finite floats; FormatFloat always produces a
// decimal that Rat.SetString can parse, including scientific notation.
func decimal(n float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(n, 'g', -1, 64))
	return r
}
