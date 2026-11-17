package llm

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"time"
)

// Rates are USD per 1M tokens. They are NEVER hardcoded: they come from
// the environment (the user's own OpenConsole bill or the provider's
// published price), and every estimate names its source. Unset rates mean
// "no rates configured" — then no estimate is shown at all, only the
// "real price unverified" label. Explicitly supplying all three zero base rates
// configures free pricing.
//
// The harness never estimates tokens locally: the only token counts it
// trusts are the ones the API returns in usage. (A local tokenizer
// would undercount — tiktoken undercounts Claude by ~15–20% — so there
// is deliberately no tokenizer in this codebase.)
type Rates struct {
	InputPerMTok      float64 // non-cached input tokens
	OutputPerMTok     float64
	CachedPerMTok     float64
	PeakInputPerMTok  float64 // 0 = same as off-peak
	PeakOutputPerMTok float64
	PeakCachedPerMTok float64
	Source            string // human-readable rate source, shown with estimates
	Configured        bool   // true when any rate is set
	// Complete means every base rate (including explicit zero) was supplied;
	// optional dollar budgets must never treat an omitted component as free.
	Complete bool
}

// RatesFromEnv reads rates from the environment:
//
//	OPENAI_PRICE_INPUT_PER_MTOK, OPENAI_PRICE_OUTPUT_PER_MTOK,
//	OPENAI_PRICE_CACHED_PER_MTOK, and the _PEAK_ variants,
//	OPENAI_PRICE_SOURCE (a label, e.g. "DeepSeek official 2026-11-03").
//
// An unparsable number is an error; unset means unconfigured.
func RatesFromEnv() (Rates, error) {
	var r Rates
	get := func(key string) (float64, error) {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return 0, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
		}
		if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("invalid %s %q: rates must be finite and nonnegative", key, v)
		}
		return f, nil
	}
	var err error
	if r.InputPerMTok, err = get("OPENAI_PRICE_INPUT_PER_MTOK"); err != nil {
		return r, err
	}
	if r.OutputPerMTok, err = get("OPENAI_PRICE_OUTPUT_PER_MTOK"); err != nil {
		return r, err
	}
	if r.CachedPerMTok, err = get("OPENAI_PRICE_CACHED_PER_MTOK"); err != nil {
		return r, err
	}
	if r.PeakInputPerMTok, err = get("OPENAI_PRICE_PEAK_INPUT_PER_MTOK"); err != nil {
		return r, err
	}
	if r.PeakOutputPerMTok, err = get("OPENAI_PRICE_PEAK_OUTPUT_PER_MTOK"); err != nil {
		return r, err
	}
	if r.PeakCachedPerMTok, err = get("OPENAI_PRICE_PEAK_CACHED_PER_MTOK"); err != nil {
		return r, err
	}
	r.Complete = true
	for _, key := range []string{"OPENAI_PRICE_INPUT_PER_MTOK", "OPENAI_PRICE_OUTPUT_PER_MTOK", "OPENAI_PRICE_CACHED_PER_MTOK"} {
		if os.Getenv(key) == "" {
			r.Complete = false
		}
	}
	r.Source = os.Getenv("OPENAI_PRICE_SOURCE")
	r.Configured = r.Complete || r.InputPerMTok > 0 || r.OutputPerMTok > 0 || r.CachedPerMTok > 0 ||
		r.PeakInputPerMTok > 0 || r.PeakOutputPerMTok > 0 || r.PeakCachedPerMTok > 0
	if r.Configured && r.Source == "" {
		r.Source = "env OPENAI_PRICE_*"
	}
	return r, nil
}

// IsPeak reports whether t falls in DeepSeek's peak window: Monday to
// Friday, 01:00–04:00 or 06:00–10:00 UTC (Beijing 09:00–12:00 and
// 14:00–18:00). Outside the window, or when no peak rates are set, the
// off-peak rates apply. The window is recorded here so estimates can be
// reconciled against a bill.
func IsPeak(t time.Time) bool {
	utc := t.UTC()
	wd := utc.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false
	}
	mins := utc.Hour()*60 + utc.Minute()
	return (mins >= 60 && mins < 240) || (mins >= 360 && mins < 600)
}

// Cost estimates the USD cost of one Usage at time t. Cached tokens are
// priced at the cached rate; the rest of the prompt at the input rate.
// Returns 0 when no rates are configured.
func (r Rates) Cost(u Usage, t time.Time) float64 {
	if !r.Configured {
		return 0
	}
	in, out, cached := r.at(t)
	nonCached := u.PromptTokens - u.CachedTokens
	if nonCached < 0 {
		nonCached = 0
	}
	return (float64(nonCached)*in + float64(u.CachedTokens)*cached + float64(u.CompletionTokens)*out) / 1e6
}

func (r Rates) at(t time.Time) (in, out, cached float64) {
	in, out, cached = r.InputPerMTok, r.OutputPerMTok, r.CachedPerMTok
	if IsPeak(t) {
		if r.PeakInputPerMTok > 0 {
			in = r.PeakInputPerMTok
		}
		if r.PeakOutputPerMTok > 0 {
			out = r.PeakOutputPerMTok
		}
		if r.PeakCachedPerMTok > 0 {
			cached = r.PeakCachedPerMTok
		}
	}
	return
}

// BudgetReady requires a complete finite rate schedule; UI estimates may use
// partial schedules, but enforcement cannot treat omitted rates as zero.
func (r Rates) BudgetReady() bool {
	if !r.Complete {
		return false
	}
	for _, v := range []float64{r.InputPerMTok, r.OutputPerMTok, r.CachedPerMTok, r.PeakInputPerMTok, r.PeakOutputPerMTok, r.PeakCachedPerMTok} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// BudgetCost applies the active schedule using exact decimal rate arithmetic.
// Unknown cache counts matter only when the active input/cache prices differ.
// The caller must separately establish that input/output counters are present.
func (r Rates) BudgetCost(u Usage, t time.Time) (*big.Rat, bool) {
	if !r.BudgetReady() || u.PromptTokens < 0 || u.CompletionTokens < 0 || u.CachedTokens < 0 || u.CachedTokens > u.PromptTokens {
		return nil, false
	}
	in, out, cached := r.at(t)
	if u.PromptTokens > 0 && !u.CacheUsageReported && in != cached {
		return nil, false
	}
	amount := new(big.Rat)
	for _, part := range []struct {
		tokens int
		rate   float64
	}{{u.PromptTokens - u.CachedTokens, in}, {u.CachedTokens, cached}, {u.CompletionTokens, out}} {
		rate, _ := new(big.Rat).SetString(strconv.FormatFloat(part.rate, 'g', -1, 64))
		amount.Add(amount, new(big.Rat).Mul(rate, new(big.Rat).SetInt64(int64(part.tokens))))
	}
	return amount.Quo(amount, big.NewRat(1000000, 1)), true
}
