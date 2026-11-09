package llm

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Rates are USD per 1M tokens. They are NEVER hardcoded: they come from
// the environment (the user's own OpenConsole bill or the provider's
// published price), and every estimate names its source. All zero means
// "no rates configured" — then no estimate is shown at all, only the
// "real price unverified" label.
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
		if f < 0 {
			return 0, fmt.Errorf("invalid %s %q: rates cannot be negative", key, v)
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
	r.Source = os.Getenv("OPENAI_PRICE_SOURCE")
	r.Configured = r.InputPerMTok > 0 || r.OutputPerMTok > 0 || r.CachedPerMTok > 0 ||
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
	in, out, cached := r.InputPerMTok, r.OutputPerMTok, r.CachedPerMTok
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
	nonCached := u.PromptTokens - u.CachedTokens
	if nonCached < 0 {
		nonCached = 0
	}
	return (float64(nonCached)*in + float64(u.CachedTokens)*cached + float64(u.CompletionTokens)*out) / 1e6
}
