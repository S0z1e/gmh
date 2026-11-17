package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// UnmarshalJSON preserves explicit zero separately from absent/null cache
// counters. It also accepts our own serialized Usage without losing unknowns.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type plain Usage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = Usage(decoded)
	var fields struct {
		Cached  *int `json:"cached_tokens"`
		Hit     *int `json:"prompt_cache_hit_tokens"`
		Details struct {
			Cached *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		Reported *bool `json:"cache_usage_reported"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields.Reported == nil {
		u.CachedTokens = 0
		for _, field := range []struct {
			source string
			value  *int
		}{
			{"cached_tokens", fields.Cached}, {"prompt_cache_hit_tokens", fields.Hit}, {"prompt_tokens_details.cached_tokens", fields.Details.Cached},
		} {
			if field.value != nil && (!u.CacheUsageReported || *field.value > u.CachedTokens) {
				u.CacheUsageReported = true
				u.CacheUsageSource = field.source
				u.CachedTokens = *field.value
			}
		}
	}
	u.normalize()
	return nil
}

func (u *responsesUsage) UnmarshalJSON(data []byte) error {
	type plain responsesUsage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = responsesUsage(decoded)
	var fields struct {
		Details struct {
			Cached *int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	u.CacheUsageReported = fields.Details.Cached != nil
	return nil
}

func (u responsesUsage) sharedUsage() Usage {
	usage := Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens, CachedTokens: u.InputTokensDetails.CachedTokens, ReasoningTokens: u.OutputTokensDetails.ReasoningTokens, CacheUsageReported: u.CacheUsageReported}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if usage.CacheUsageReported {
		usage.CacheUsageSource = "input_tokens_details.cached_tokens"
	}
	usage.normalize()
	return usage
}

func (u *anthropicUsage) UnmarshalJSON(data []byte) error {
	type plain anthropicUsage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = anthropicUsage(decoded)
	var fields struct {
		Cached   *int `json:"cache_read_input_tokens"`
		Input    *int `json:"input_tokens"`
		Output   *int `json:"output_tokens"`
		Creation *int `json:"cache_creation_input_tokens"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	u.CacheUsageReported = fields.Cached != nil
	u.hasInput = fields.Input != nil
	u.hasOutput = fields.Output != nil
	u.hasCreation = fields.Creation != nil
	return nil
}

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
	// Programmatically supplied counters predate presence metadata. Positive
	// counts are evidence, except an explicitly partial aggregate.
	if u.CachedTokens > 0 && u.CacheUsageSource != "partial" {
		u.CacheUsageReported = true
	}
	if u.CacheUsageReported {
		u.CacheReportedPromptTokens = u.PromptTokens
	}
}

// Add accumulates another Usage into this one (session totals).
func (u *Usage) Add(o Usage) {
	o.normalize()
	if o.PromptTokens != 0 || o.CompletionTokens != 0 || o.TotalTokens != 0 {
		if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
			u.CacheUsageReported = o.CacheUsageReported
			u.CacheUsageSource = o.CacheUsageSource
		} else if !u.CacheUsageReported || !o.CacheUsageReported {
			if u.CacheUsageReported || o.CacheUsageReported || u.CacheUsageSource == "partial" || o.CacheUsageSource == "partial" {
				u.CacheUsageSource = "partial"
			}
			u.CacheUsageReported = false
		} else if u.CacheUsageSource != o.CacheUsageSource {
			u.CacheUsageSource = "mixed"
		}
	}
	u.PromptTokens += o.PromptTokens
	u.CacheReportedPromptTokens += o.CacheReportedPromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
	u.CachedTokens += o.CachedTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// String renders usage the way the CLI prints it. Reasoning tokens
// are a subset of completion, so they are shown as an extra figure
// (only when non-zero) and never added to any total.
func (u Usage) String() string {
	u.normalize()
	cached := fmt.Sprint(u.CachedTokens)
	if !u.CacheUsageReported {
		cached = "unknown"
		if u.CachedTokens > 0 {
			cached = fmt.Sprintf("%d+unknown", u.CachedTokens)
		}
	}
	s := fmt.Sprintf("prompt=%d (cached=%s) completion=%d total=%d",
		u.PromptTokens, cached, u.CompletionTokens, u.TotalTokens)
	if u.ReasoningTokens > 0 {
		s += fmt.Sprintf(" reasoning=%d", u.ReasoningTokens)
	}
	return s
}

// traceWireUsage preserves missing/null scalar counters separately from zero.
// A reported total can establish tokens even when the price breakdown is absent.
func traceWireUsage(ctx context.Context, usage Usage, data string, responses bool) {
	if observer, _ := ctx.Value(traceKey{}).(*requestObservation); observer == nil {
		return
	}
	var envelope struct {
		Type     string          `json:"type"`
		Usage    json.RawMessage `json:"usage"`
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	_ = json.Unmarshal([]byte(data), &envelope)
	raw := envelope.Usage
	if responses {
		raw = envelope.Response.Usage
	}
	var counts struct {
		Prompt     *int `json:"prompt_tokens"`
		Completion *int `json:"completion_tokens"`
		Input      *int `json:"input_tokens"`
		Output     *int `json:"output_tokens"`
		Total      *int `json:"total_tokens"`
	}
	_ = json.Unmarshal(raw, &counts)
	input, output := counts.Prompt, counts.Completion
	if responses {
		input, output = counts.Input, counts.Output
	}
	complete := input != nil && output != nil
	final := !responses || envelope.Type == "response.completed" || envelope.Type == "response.incomplete"
	traceUsage(ctx, usage, final && (counts.Total != nil || complete), final && complete)
}
