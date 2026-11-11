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
	if u.CompletionTokensDetails.ReasoningTokens > u.ReasoningTokens {
		u.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
}

// Add accumulates another Usage into this one (session totals).
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
	u.CachedTokens += o.CachedTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// String renders usage the way the CLI prints it. Reasoning tokens
// are a subset of completion, so they are shown as an extra figure
// (only when non-zero) and never added to any total.
func (u Usage) String() string {
	s := fmt.Sprintf("prompt=%d (cached=%d) completion=%d total=%d",
		u.PromptTokens, u.CachedTokens, u.CompletionTokens, u.TotalTokens)
	if u.ReasoningTokens > 0 {
		s += fmt.Sprintf(" reasoning=%d", u.ReasoningTokens)
	}
	return s
}
