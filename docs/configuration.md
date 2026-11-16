# Configuration file (JSON)

Model setup lives in a JSON file instead of a dozen exports, following
the `models.json` shape.
Two files, merged field by field after the workspace is explicitly trusted
(project wins where it sets a value):

- global: `~/.minimal-harness/config.json`
- project: `<root>/.harness.json`

Workspaces start untrusted. Run `harness --workspace-trust trust` (or the
same flag with `harness-tui`) to save trust for this canonical directory.
Future runs reuse the decision. `--workspace-trust untrust` revokes it.
Decisions live under `~/.minimal-harness/trust/`, bind the directory identity,
and do not apply to child directories or a replacement directory.

Untrusted workspaces use global configuration; project config, plugins,
project instructions, project skills and startup Git commands are disabled.
Explicit CLI flags and `--config <path>` remain direct user choices; an
explicit config does not enable project plugins. Trust takes effect on the
next process start. It allows project configuration and executable plugins;
it does not create an OS sandbox. RPC `get_state.workspace_trusted` reports
the active decision.

```json
{
  "defaultProvider": "example-anthropic",
  "defaultModel": "example-reasoning-model",
  "providers": {
    "example-chat": {
      "baseUrl": "https://api.example.com/v1",
      "api": "openai-completions",
      "apiKey": "$EXAMPLE_API_KEY",
      "models": [
        {"id": "example-chat-model", "contextWindow": 1000000, "maxTokens": 16384,
         "input": ["text", "image"],
         "cost": {"input": 0.2, "output": 0.8, "cacheRead": 0.05, "cacheWrite": 0.2},
         "samplingParams": {"temperature": 0.7}}
      ]
    },
    "example-anthropic": {
      "baseUrl": "https://api.example.org/v1",
      "api": "anthropic-messages",
      "apiKey": "$EXAMPLE_API_KEY",
      "models": [
        {"id": "example-reasoning-model", "reasoning": true, "contextWindow": 256000, "maxTokens": 16384,
         "input": ["text", "image"]},
        {"id": "example-chat-model-2", "api": "openai-completions"}
      ]
    }
  }
}
```

A provider must be configured by the user — there is no built-in
default provider. With no config file, or a config with no provider,
setup fails with "no providers configured". A new provider needs a
`baseUrl` and an `api` at provider or model level.

Each `models` entry's only required field is `id`. The rest is
optional, with defaults: `contextWindow` 128000 (drives the
compact trigger), `maxTokens` 16384, `reasoning` false,
`input` `["text"]`. The rest has no default: `name`, `api` and
`baseUrl` (override the provider's), `effort`, `samplingParams`, `headers`.
`api` accepts the long spellings and the harness short names:
`openai-completions` = `chat`, `openai-responses` = `responses`,
`anthropic-messages` = `anthropic`; `google-generative-ai` is
rejected — the harness has no client for it.

`apiKey` accepts a literal key or a `"$NAME"` / `"${NAME}"` environment
reference. An unset referenced variable is an error; `!command` is not supported.
When a provider sets no `apiKey`, `OPENAI_API_KEY` is used.
`headers` (provider and per-model, model wins) accept the same
`$NAME` references; auth headers are never taken from them.

`samplingParams` (e.g. `temperature`, `top_p`) is merged into every
request body of the two OpenAI modes only; Anthropic
ignores it. Structural keys (`model`, `messages`, `tools`, `stream`,
`max_tokens`, …) are never overridden by it.

For Anthropic, the optional per-model `thinking` selects `adaptive` or
`enabled` when `effort` is set. By default known Claude 4.6+ IDs use adaptive
thinking plus `output_config.effort`; older Claude and unknown gateway IDs
retain manual budgets. Set `thinking` explicitly for model aliases.
Manual thinking requires `maxTokens` greater than 1024. Anthropic response
blocks (including opaque signatures) are persisted for replay through tool
loops; signatures and redacted data are never rendered as text.

`effort` (`low`, `medium` or `high`; empty means the provider
default) is how hard a reasoning model thinks, sent per API shape:
chat sends `reasoning_effort`, responses sends
`reasoning: {"effort": ...}`, and Anthropic sends
`thinking: {"type": "enabled", "budget_tokens": ...}` with a budget
of maxTokens/16, /4 or /2 (at least 1024 tokens, always below
maxTokens — with the default maxTokens of 16384 that is 1024, 4096
or 8192). A model that does not reason ignores it. `effort` is
per-model, so switching models with `/model` (or RPC `set_model`)
carries each model's own effort. `maxTokens` is sent by
all three modes (chat: `max_tokens`, responses: `max_output_tokens`,
anthropic: `max_tokens`). `cost` (USD per million tokens, all four
rates required when present, plus optional `tiers`) is parsed and
stored only — the cost estimate still comes from the `OPENAI_PRICE_*`
environment rates. `input` declares what the model accepts
(`["text"]`, or `["text", "image"]` for image input — see
[images](images.md)).

Merge rules: a project file's `models` list replaces the provider's
whole list; a `baseUrl`/`headers`-only override keeps the
provider's models. Values are validated on the merged result, not
per file. Unknown fields are rejected, so a typo fails fast. A
`"$schema"` key is accepted and ignored. The top-level `baseUrl` /
`api` / `model` fields are a shorthand for a single provider without
the `providers` map. Other top-level knobs: `mode`
(`suggest|auto`), `bashTimeoutSeconds`, `bgThresholdSeconds` (`0`
disables backgrounding), `denyInsteadOfPrompt`, `contextLimit`,
`anthropicMaxTokens`, `tuiTheme`
(`default|terminal|catppuccin-mocha`). Precedence is flag >
environment variable > model value > project file > global file > the
default, so existing flag/env usage keeps working unchanged.

With that file in place, `harness` needs nothing but the keys — no
`OPENAI_BASE_URL` / `OPENAI_MODEL` / `OPENAI_API_MODE` exports.
Switch providers without editing the file:
`harness -provider example-chat`. Switch models:
`harness -model example-chat-model-2`. Point at one file
explicitly: `harness -config /path/to/config.json`.
