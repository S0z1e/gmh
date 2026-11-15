package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageNormalizePicksCachedFromDetails(t *testing.T) {
	var u Usage
	raw := `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80}}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	u.normalize()
	if u.CachedTokens != 80 {
		t.Fatalf("cached = %d, want 80", u.CachedTokens)
	}
}

func TestCacheUsagePresenceAndRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		reported  bool
		source    string
	}{
		{"missing", `{}`, false, ""},
		{"null", `{"cached_tokens":null,"prompt_tokens_details":{"cached_tokens":null}}`, false, ""},
		{"zero", `{"cached_tokens":0}`, true, "cached_tokens"},
		{"hit_zero", `{"prompt_cache_hit_tokens":0}`, true, "prompt_cache_hit_tokens"},
		{"details_zero", `{"prompt_tokens_details":{"cached_tokens":0}}`, true, "prompt_tokens_details.cached_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u Usage
			if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
				t.Fatal(err)
			}
			if u.CacheUsageReported != tc.reported || u.CacheUsageSource != tc.source {
				t.Fatalf("usage=%+v", u)
			}
			b, _ := json.Marshal(u)
			var restored Usage
			if err := json.Unmarshal(b, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.CacheUsageReported != tc.reported {
				t.Fatal("round trip lost presence")
			}
			if !tc.reported && !strings.Contains(u.String(), "cached=unknown") {
				t.Fatal(u.String())
			}
		})
	}
}

func TestCacheUsagePartialAggregatesRemainPartial(t *testing.T) {
	known := Usage{PromptTokens: 100, TotalTokens: 101, CacheUsageReported: true, CacheUsageSource: "cached_tokens", CachedTokens: 80}
	unknown := Usage{PromptTokens: 50, TotalTokens: 51}
	for _, items := range [][]Usage{{known, unknown}, {unknown, known}} {
		var total Usage
		for _, u := range items {
			total.Add(u)
		}
		if total.CacheUsageReported || total.CacheUsageSource != "partial" || total.CacheReportedPromptTokens != 100 {
			t.Fatalf("total=%+v", total)
		}
		b, _ := json.Marshal(total)
		var restored Usage
		if err := json.Unmarshal(b, &restored); err != nil {
			t.Fatal(err)
		}
		var session Usage
		session.Add(restored)
		if session.CacheUsageReported || session.CacheReportedPromptTokens != 100 || !strings.Contains(session.String(), "cached=80+unknown") {
			t.Fatalf("session=%+v %s", session, session.String())
		}
	}
	var total Usage
	total.Add(Usage{})
	total.Add(known)
	total.Add(Usage{})
	if !total.CacheUsageReported || total.CacheReportedPromptTokens != 100 {
		t.Fatalf("empty usage changed coverage: %+v", total)
	}
}

func TestCacheUsageOtherAPIsPresence(t *testing.T) {
	for _, reported := range []bool{false, true} {
		responses := `{"input_tokens":100}`
		anthropic := `{"input_tokens":100}`
		if reported {
			responses = `{"input_tokens":100,"input_tokens_details":{"cached_tokens":0}}`
			anthropic = `{"input_tokens":100,"cache_read_input_tokens":0}`
		}
		var r responsesUsage
		var a anthropicUsage
		if err := json.Unmarshal([]byte(responses), &r); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(anthropic), &a); err != nil {
			t.Fatal(err)
		}
		if r.CacheUsageReported != reported || a.CacheUsageReported != reported {
			t.Fatalf("reported=%v responses=%+v anthropic=%+v", reported, r, a)
		}
	}
}

func TestUsageNormalizePicksCachedTopLevel(t *testing.T) {
	var u Usage
	raw := `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":60,"prompt_cache_hit_tokens":55}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	u.normalize()
	if u.CachedTokens != 60 {
		t.Fatalf("cached = %d, want 60 (max of sources)", u.CachedTokens)
	}
}

func TestUsageAddAndString(t *testing.T) {
	var total Usage
	total.Add(Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CachedTokens: 80})
	total.Add(Usage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60, CachedTokens: 40})
	if total.PromptTokens != 150 || total.CompletionTokens != 30 || total.TotalTokens != 180 || total.CachedTokens != 120 {
		t.Fatalf("accumulated usage = %+v", total)
	}
	want := "prompt=150 (cached=120) completion=30 total=180"
	if got := total.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestResponsesUsageCached(t *testing.T) {
	var u responsesUsage
	raw := `{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":70}}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	if u.InputTokensDetails.CachedTokens != 70 {
		t.Fatalf("cached = %d, want 70", u.InputTokensDetails.CachedTokens)
	}
}
