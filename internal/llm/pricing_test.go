package llm

import (
	"testing"
	"time"
)

func TestIsPeak(t *testing.T) {
	// 2026-11-09 is a Monday.
	cases := []struct {
		when time.Time
		want bool
	}{
		{time.Date(2026, 11, 9, 0, 59, 0, 0, time.UTC), false}, // before first window
		{time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC), true},   // 01:00 start
		{time.Date(2026, 11, 9, 3, 59, 0, 0, time.UTC), true},  // inside first window
		{time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC), false},  // 04:00 end
		{time.Date(2026, 11, 9, 5, 30, 0, 0, time.UTC), false}, // between windows
		{time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC), true},   // 06:00 start
		{time.Date(2026, 11, 9, 9, 59, 0, 0, time.UTC), true},  // inside second window
		{time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC), false}, // 10:00 end
		{time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), false}, // midday
		{time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC), false}, // Saturday 02:00
		{time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC), false}, // Sunday 07:00
		// Same instant in another zone must give the same answer.
		{time.Date(2026, 11, 9, 17, 0, 0, 0, time.FixedZone("CST", 8*3600)), true}, // 09:00 UTC
	}
	for _, tc := range cases {
		if got := IsPeak(tc.when); got != tc.want {
			t.Fatalf("IsPeak(%v) = %v, want %v", tc.when, got, tc.want)
		}
	}
}

func TestRatesCost(t *testing.T) {
	r := Rates{InputPerMTok: 0.30, OutputPerMTok: 1.20, CachedPerMTok: 0.006, Configured: true}
	offPeak := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	u := Usage{PromptTokens: 1000000, CachedTokens: 500000, CompletionTokens: 100000, TotalTokens: 1100000}
	// (500000*0.30 + 500000*0.006 + 100000*1.20) / 1e6 = 0.273
	if got := r.Cost(u, offPeak); got != 0.273 {
		t.Fatalf("Cost = %v, want 0.273", got)
	}
	// Unconfigured rates cost nothing.
	if got := (Rates{}).Cost(u, offPeak); got != 0 {
		t.Fatalf("unconfigured Cost = %v, want 0", got)
	}
	// Cached tokens never exceed the prompt: clamp the non-cached part.
	odd := Usage{PromptTokens: 100, CachedTokens: 200, CompletionTokens: 0}
	if got := r.Cost(odd, offPeak); got != 200*0.006/1e6 {
		t.Fatalf("Cost with cached>prompt = %v", got)
	}
}

func TestRatesCostPeak(t *testing.T) {
	r := Rates{
		InputPerMTok: 0.15, OutputPerMTok: 0.60, CachedPerMTok: 0.003,
		PeakInputPerMTok: 0.30, PeakOutputPerMTok: 1.20, PeakCachedPerMTok: 0.006,
		Configured: true,
	}
	u := Usage{PromptTokens: 1000000, CachedTokens: 0, CompletionTokens: 100000, TotalTokens: 1100000}
	peak := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	// Peak: (1000000*0.30 + 100000*1.20) / 1e6 = 0.42
	if got := r.Cost(u, peak); got != 0.42 {
		t.Fatalf("peak Cost = %v, want 0.42", got)
	}
	offPeak := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	// Off-peak: (1000000*0.15 + 100000*0.60) / 1e6 = 0.21
	if got := r.Cost(u, offPeak); got != 0.21 {
		t.Fatalf("off-peak Cost = %v, want 0.21", got)
	}
	// Peak rates unset: peak hours fall back to off-peak rates.
	r2 := Rates{InputPerMTok: 0.15, OutputPerMTok: 0.60, CachedPerMTok: 0.003, Configured: true}
	if got := r2.Cost(u, peak); got != 0.21 {
		t.Fatalf("peak without peak rates = %v, want 0.21", got)
	}
}

func TestRatesFromEnv(t *testing.T) {
	t.Setenv("OPENAI_PRICE_INPUT_PER_MTOK", "0.30")
	t.Setenv("OPENAI_PRICE_OUTPUT_PER_MTOK", "1.20")
	t.Setenv("OPENAI_PRICE_CACHED_PER_MTOK", "0.006")
	t.Setenv("OPENAI_PRICE_SOURCE", "DeepSeek official 2026-11-03")
	r, err := RatesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Configured || r.InputPerMTok != 0.30 || r.Source != "DeepSeek official 2026-11-03" {
		t.Fatalf("RatesFromEnv = %+v", r)
	}
	// Default source label when rates are set without one.
	t.Setenv("OPENAI_PRICE_SOURCE", "")
	r, err = RatesFromEnv()
	if err != nil || r.Source != "env OPENAI_PRICE_*" {
		t.Fatalf("default source = %q, err = %v", r.Source, err)
	}
}

func TestRatesFromEnvErrors(t *testing.T) {
	t.Setenv("OPENAI_PRICE_INPUT_PER_MTOK", "not-a-number")
	if _, err := RatesFromEnv(); err == nil {
		t.Fatal("unparsable rate should error")
	}
	t.Setenv("OPENAI_PRICE_INPUT_PER_MTOK", "-1")
	if _, err := RatesFromEnv(); err == nil {
		t.Fatal("negative rate should error")
	}
}
