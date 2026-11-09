package llm

import (
	"encoding/json"
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
