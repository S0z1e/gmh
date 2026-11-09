package llm

import "fmt"

// normalize picks the cached-token count from whichever field the
// provider actually returned. Different providers put it in different
// places; they never disagree, so the max is the right one.
func (u *Usage) normalize() {
	if u.PromptCacheHitTokens > u.CachedTokens {
		u.CachedTokens = u.PromptCacheHitTokens
	}
	if u.PromptTokensDetails.CachedTokens > u.CachedTokens {
		u.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
}

// Add accumulates another Usage into this one (session totals).
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
	u.CachedTokens += o.CachedTokens
}

// String renders usage the way the CLI prints it.
func (u Usage) String() string {
	return fmt.Sprintf("prompt=%d (cached=%d) completion=%d total=%d",
		u.PromptTokens, u.CachedTokens, u.CompletionTokens, u.TotalTokens)
}
