package llm

// ToolInvocation is the durable execution identity of one tool call. The model's
// original assistant record remains in the transcript; checkpoints also retain
// this provenance after replacing the replay arguments with the effective ones.
// Provider wire adapters deliberately omit this harness-only metadata.
type ToolInvocation struct {
	ID                string            `json:"id"`
	CallIndex         int               `json:"call_index"`
	ToolCallID        string            `json:"tool_call_id"`
	ToolName          string            `json:"tool_name"`
	OriginalArguments string            `json:"original_arguments"`
	Arguments         string            `json:"arguments"`
	TargetPath        string            `json:"target_path,omitempty"`
	Rewrites          []ArgumentRewrite `json:"rewrites,omitempty"`
}

type ArgumentRewrite struct {
	Source    string `json:"source"` // plugin_hook or continuation
	Arguments string `json:"arguments"`
}
