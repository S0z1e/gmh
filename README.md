# minimal-harness

A minimal, Pi-light coding agent harness in Go. The core (CLI, agent
loop, tools, sessions, config, RPC) is stdlib-only; the TUI binary
adds Bubble Tea v2 + lipgloss v2. Seven tools: read, write, edit,
bash, grep, glob, `load_skill`. Usage is shown per turn and per
session (prompt, cached, completion, total tokens), always from the
API's returned usage — never estimated locally (no local tokenizer).
Cost is estimated only when you set rates:
`OPENAI_PRICE_INPUT_PER_MTOK`, `OPENAI_PRICE_OUTPUT_PER_MTOK`,
`OPENAI_PRICE_CACHED_PER_MTOK` (USD per 1M tokens), plus `_PEAK_`
variants and `OPENAI_PRICE_SOURCE` to label the source. Peak hours
are Mon–Fri 01:00–04:00 and 06:00–10:00 UTC; without peak rates the
off-peak rates apply. Every estimate is labelled with its source and
"`<Provider>` 實價未驗證"; without rates, no estimate is shown.

## Quickstart

```bash
# 1. Write ~/.minimal-harness/config.json (see Configuration below).
#    It names your providers and models; the keys stay in env vars.
# 2. Run it.
go run ./cmd/harness -root /path/to/project

# One task, headless, then exit:
go run ./cmd/harness -p -root /path/to/project "look at this project and add a test"

# Resume a session:
go run ./cmd/harness --resume <session-id> -root /path/to/project
```

Flags and env vars override the config file (flag > env > model >
project file > global file > default). `OPENAI_BASE_URL`,
`OPENAI_MODEL` and `OPENAI_API_MODE` still work as overrides;
`-provider`, `-model`, `-api` and `-config` pick the provider, model,
API shape, or config file for one run.

## Safety (suggest mode) — what it can and cannot promise

Approval is a policy boundary, **not a sandbox**. In `-mode suggest`,
write/edit/bash ask first (stdin in plain mode, a y/n dialog in the
TUI); read, grep, glob and `load_skill` never ask. `-p` print mode
and `--deny-instead-of-prompt` (also `OPENAI_DENY_INSTEAD_OF_PROMPT=1`)
refuse every approval without asking — they only ever deny, never
approve, and the turn continues with the denial as an observation.

What is enforced:
- read/write/edit/grep/glob paths are jailed to the workspace root
  (`..` escapes refused); bash starts in the root.
- bash has a timeout (default 60s, per-call up to 300s,
  `-bash-timeout` / `OPENAI_BASH_TIMEOUT_SECONDS`) and a 64 KiB
  output cap.
- A denylist refuses the obvious catastrophes before they run:
  `rm -rf /` (or `~`), `mkfs` with a device, `dd` to a device,
  writes into `~/.ssh` / `~/.aws` / `~/.config/gcloud` / `~/.kube` /
  `~/.docker`, and anything touching the Docker socket.
- Every approval decision, every bash run (with exit code), and every
  denial is written to `<session>-audit.jsonl` next to the session
  transcript, for later review.

What is NOT promised (residual risk — run on disposable copies or in
a container):
- The denylist is pattern matching over the command text; it can be
  bypassed with variables, quoting, or indirection.
- Bash is only started in the root — once a bash call is approved it
  can `cd` out and write anywhere, including paths the write/edit
  tools refuse. Denying write/edit does not stop bash from writing.
- There is no OS sandbox (no bwrap / sandbox-exec); the denylist and
  jail are harm reduction, not isolation.

## Background bash (long commands don't block the loop)

A bash call that runs longer than the background threshold (default
10s, `-bg-threshold` / `OPENAI_BG_THRESHOLD_SECONDS`; `0` disables
it) keeps running as a job instead of blocking the model: the model
gets a "still running in the background (job bg-1)" observation and
continues with other work. When the model stops calling tools, the
loop waits for the running jobs and feeds each result back as a user
message ("Background job bg-1 finished (exit 0). Output: …"), so the
model can act on it. The result is a user message, not a second tool
message for the same call id — the call already has its tool result,
and a second one with the same id is a 400 from the API. A bash call
that finishes within the threshold never becomes a job. `/jobs` lists
the background jobs (id, status, elapsed, command, exit code once
done). The denylist, timeout, 64 KiB output cap and audit log apply
to background jobs exactly as to synchronous ones. `-p` print mode
never backgrounds (it stays synchronous). If the step cap hits (or
the API call fails) with jobs still running, their results are still
delivered to the session before the error is reported, capped at
32 KiB head+tail; quitting with jobs running warns that they are
abandoned (their child processes keep running, orphaned).

## Images

The harness can send images to a model that declares image input
(`"input": ["text", "image"]` in the config). Three ways in:
- `-image <path>` (repeatable) attaches images to the first turn,
  in `-p` or the REPL.
- `/image <path>` in the REPL attaches to your next message
  (`/image` alone lists what is attached).
- The model itself can `read` an image file (png, jpg, gif, webp):
  the read returns a marker, and the image is attached to the next
  request as a user message (tool messages cannot carry images in
  any of the three APIs).

The model receives the image in its own format: chat gets an
`image_url` data URI, Responses gets `input_image`, Anthropic gets a
base64 image block. One image is capped at 10 MiB, ten per message.
Sessions store the image path, never the bytes; on `--resume` the
bytes are re-read from disk, so a deleted file leaves only the marker
text. A model without `"image"` in its `input` refuses the attach
with an error that says what to add. The GUI route is the RPC
`attach_image` request (see below).

## Layout

```
cmd/harness/main.go      // CLI: -p print mode, REPL, --resume, -mode suggest/auto
internal/llm/client.go   // OpenAI-compatible client, streaming, tool-call accumulation
internal/llm/image.go    // Image type + wire parts for chat/anthropic/responses
internal/tools/image.go  // LoadImage: workspace jail, type/size caps, dimensions
internal/agent/loop.go   // single loop: LLM -> tool -> observe -> repeat
internal/tools/tools.go  // read/write/edit/bash, workspace-root jail, timeout
internal/tools/grep.go   // grep tool (ripgrep backend, grep -r fallback, match cap + truncation)
internal/tools/glob.go   // glob tool (rg --files backend, WalkDir fallback, limit + truncation)
internal/session/session.go // JSONL append + resume
internal/config/config.go   // JSON config file: providers, models, merge (stdlib only)
internal/skills/skills.go  // .agents/skills/*/SKILL.md discovery, listing only
internal/rpc/server.go     // RPC mode: JSONL over stdio for a GUI (see below)
internal/rpc/protocol.go   // RPC request/response/event JSON types
internal/agent/prompts/system.md // lightweight system prompt, embedded
```

## Run

- The API key is read only from the environment. Never commit it,
  never put it in a file.
- `OPENAI_BASE_URL` may be given with or without a trailing `/v1`;
  the client normalises it.
- Three API shapes: `-api chat` (`POST /v1/chat/completions`,
  messages, `tool_calls[]`); `-api responses` (`POST /v1/responses`,
  instructions + input items, `function_call` /
  `function_call_output`, `store: false` — stateless, JSONL is the
  store; not every provider/model supports it, and a rejection is
  returned verbatim); `-api anthropic` (`POST /v1/messages`, system
  string + messages with `tool_use` / `tool_result` blocks). Auth is
  picked by host: `api.anthropic.com` gets `x-api-key` only; every
  other host gets Bearer only — sending both breaks Discovery, which
  prefers `x-api-key`. History is translated at call time, so
  sessions stay in the shared format and replay across modes;
  consecutive same-role messages merge into one (Anthropic requires
  strict user/assistant alternation), an empty tool result replays as
  `(no output)`, and thinking blocks are skipped like
  `reasoning_content` in chat mode. `max_tokens` defaults to 16384
  (Pi's default), or `ANTHROPIC_MAX_TOKENS`.
- Stream integrity: a chat stream that ends without `[DONE]`, a
  Responses stream without `response.completed`, or an Anthropic
  stream without `message_stop`, is an error, not a silently
  truncated reply. A Responses `response.completed` that lists only
  some of the calls the stream carried no longer drops the rest: the
  missing ones are merged from the accumulated fragments. Chat
  streams request `stream_options: {"include_usage": true}` — without
  it a spec-compliant provider sends no usage object at all and the
  turn reports 0 tokens.
- Modes: `-mode suggest` (default, ask before write/edit/bash),
  `-mode auto` (no prompts, use only on a disposable copy).
- Skills: put `SKILL.md` files under `.agents/skills/<name>/` in the
  project, or `~/.config/minimal-harness/skills/`. Only name +
  description are loaded at startup; the full text loads on demand
  three ways: the user types `/skill:<name>`, the task names a skill
  as a whole word (auto-loaded before the turn), or the model calls
  the `load_skill` tool when a task matches a skill. A skill's
  `references/` files are never loaded up front: `load_skill` lists
  them in a footer, and the model loads one with
  `load_skill {name, reference}` (paths are jailed to that skill's
  `references/`).
- Sessions live in `~/.minimal-harness/sessions/<id>.jsonl`.
- REPL commands: `/usage` shows session token totals, `/clear` drops
  the conversation context (a marker is written to the session file;
  `--resume` starts from the clear point, older messages stay in the
  file). `/model` lists the configured models; `/model <provider>/<model>`
  (or a bare model id) switches model mid-session — the client is
  rebuilt from that model's config (api, baseUrl, maxTokens,
  contextWindow, image input) and history replays through the new
  API on the next turn. The switch lasts for the session only; it
  does not rewrite the config file.
- Compaction: `/compact` summarises the conversation into a 6-section
  brief and replaces history with it; the original transcript is
  saved to `<session>-pre-compact-<time>.jsonl`. It also triggers
  automatically when the last call's context reaches 70% of the
  window (`-context-limit`, default: the model's `contextWindow`,
  else 128000).
- Events: the agent loop emits an event stream (`text_delta`,
  `step_usage`, `tool_start`, `tool_end`, `done`, `error`); the CLI
  is one renderer of it, the TUI and the RPC server are two more. The
  loop never imports UI code.
- UI selection (`--ui auto|plain|tui`, default `auto`): precedence is
  flag > `NO_COLOR` (non-empty) > `TERM=dumb` > TTY. On a terminal,
  `auto` opens the TUI; piped, `NO_COLOR`, or `TERM=dumb` means plain.
  `-p` is always plain and ignores `--ui`. `harness --ui tui` without
  a terminal falls back to plain with a warning. The TUI is a separate
  binary (`harness-tui` next to `harness`); `harness` re-execs it
  when the TUI is selected, so `-p` / the plain CLI never import
  Bubble Tea.

### TUI

Inline (no alt-screen, so shell scrollback still works), opencode-style:
dark palette with peach `#fab283` and purple `#9d7cd8` accents; user
turns are raised panels with a peach `┃` left border; assistant text
is markdown-rendered and indented; tool rows are muted
(`✓ Read t.txt`, `✗ Write u.txt`); bash shows a raised block with the
command in the top border (`┤ $ <cmd> ├`) and an output tail (≤10
lines). While working, a braille spinner shows the pending tool label
in plain words (`Reading src/foo.ts…`), or `Thinking…` — but only
after the turn has run 300 ms with no text, so short turns never
flash a spinner. Usage lines stay in the timeline with thousands
separators. The approval dialog is a full-width panel:
`△ Approval required`, the tool summary, `[y] Allow once` on a solid
blue background, `[n] Deny`, `esc = deny` (single key, Esc = deny).
The composer is a `┃` input line with a meta line: left is the mode
chip · model · api, right is the workspace git branch and the session
totals (`3 turns · 12,345 tokens`). Themes:
`HARNESS_TUI_THEME=default|terminal|catppuccin-mocha`; `terminal`
paints no raised background, `catppuccin-mocha` is the Catppuccin
Mocha palette. Screenshots (real captures):
`harness-tui-screenshot.png` (real model), `harness-tui-approval.png`
(approval dialog).

## Configuration file (JSON, Pi-style)

Model setup lives in a JSON file instead of a dozen exports, following
Pi's `models.json` shape (see pi.dev/docs/latest/models-configuration).
Two files, merged field by field (project wins where it sets a value):

- global: `~/.minimal-harness/config.json`
- project: `<root>/.harness.json`

```json
{
  "defaultProvider": "discovery",
  "defaultModel": "Agents-A1",
  "providers": {
    "openconsole": {
      "baseUrl": "https://www.openconsole.cn/v1",
      "api": "openai-completions",
      "apiKey": "$OPENAI_API_KEY",
      "models": [
        {"id": "deepseek-v4.1-flash", "contextWindow": 1000000, "maxTokens": 16384,
         "input": ["text", "image"],
         "cost": {"input": 0.2, "output": 0.8, "cacheRead": 0.05, "cacheWrite": 0.2},
         "samplingParams": {"temperature": 0.7}}
      ]
    },
    "discovery": {
      "baseUrl": "https://discovery-api.intern-ai.org.cn/v1",
      "api": "anthropic-messages",
      "apiKey": "$DISCOVERY_API_KEY",
      "models": [
        {"id": "Agents-A1", "reasoning": true, "contextWindow": 256000, "maxTokens": 16384,
         "input": ["text", "image"]},
        {"id": "deepseek-v4-flash-0731", "api": "openai-completions"}
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
optional, with Pi defaults: `contextWindow` 128000 (drives the
compact trigger), `maxTokens` 16384, `reasoning` false,
`input` `["text"]`. The rest has no default: `name`, `api` and
`baseUrl` (override the provider's), `samplingParams`, `headers`.
`api` accepts Pi spellings and the harness short names:
`openai-completions` = `chat`, `openai-responses` = `responses`,
`anthropic-messages` = `anthropic`; `google-generative-ai` is
rejected — the harness has no client for it.

`apiKey` is a `"$NAME"` / `"${NAME}"` environment reference only: a
literal key in the file is refused, and `!command` is not supported.
When a provider sets no `apiKey`, `OPENAI_API_KEY` is used.
`headers` (provider and per-model, model wins) accept the same
`$NAME` references; auth headers are never taken from them.

`samplingParams` (e.g. `temperature`, `top_p`) is merged into every
request body of the two OpenAI modes only — Pi's rule; Anthropic
ignores it. Structural keys (`model`, `messages`, `tools`, `stream`,
`max_tokens`, …) are never overridden by it. `maxTokens` is sent by
all three modes (chat: `max_tokens`, responses: `max_output_tokens`,
anthropic: `max_tokens`). `cost` (USD per million tokens, all four
rates required when present, plus optional `tiers`) is parsed and
stored only — the cost estimate still comes from the `OPENAI_PRICE_*`
environment rates. `input` is stored only: the harness is text-only
for now, so a model can declare `["text", "image"]` but no image is
sent yet.

Merge rules: a project file's `models` list replaces the provider's
whole list (Pi-style); a `baseUrl`/`headers`-only override keeps the
provider's models. Values are validated on the merged result, not
per file. Unknown fields are rejected, so a typo fails fast. A
`"$schema"` key is accepted and ignored. The top-level `baseUrl` /
`api` / `model` fields are a shorthand for a single provider without
the `providers` map. Other top-level knobs: `mode`
(`suggest|auto`), `bashTimeoutSeconds`, `bgThresholdSeconds` (`0`
disables backgrounding), `denyInsteadOfPrompt`, `contextLimit`,
`anthropicMaxTokens`, `tuiTheme`
(`default|terminal|catppuccin-mocha`). Precedence is flag >
environment variable > model value > project file > global file > Pi
default, so existing flag/env usage keeps working unchanged.

With that file in place, `harness` needs nothing but the keys — no
`OPENAI_BASE_URL` / `OPENAI_MODEL` / `OPENAI_API_MODE` exports.
Switch providers without editing the file:
`harness -provider openconsole`. Switch models:
`harness -model deepseek-v4-flash-0731`. Point at one file
explicitly: `harness -config /path/to/config.json`.

## RPC mode (JSONL over stdio, for a GUI)

`harness --rpc` is the machine frontend: one JSON object per line on
stdin (requests) and stdout (responses and events), LF terminated,
split on LF only — the same framing as `pi --mode rpc`, so a GUI
written against Pi's protocol needs only small changes. stdout
carries protocol data only; diagnostics go to stderr. `-p` conflicts
with `--rpc`; `--ui` is ignored.

```bash
harness --rpc -root /path/to/project -mode suggest
```

Startup: the first line on stdout is a `ready` event:
`{"type":"ready","protocol":1,"session":"<id>","model":"...","api":"chat","mode":"suggest","root":"...","resumed":false,"messages":0}`.

Requests (each `{"id":"<req>","type":"..."}`; `id` is optional and
echoed in the response):
- `prompt` `{"message":"..."}` → `{"disposition":"started"|"queued"}`.
  The response only means accepted; the turn ends with
  `agent_settled`. A second prompt while a turn runs is queued (cap
  64; beyond that an error). A known slash line (`/usage`, `/jobs`,
  `/clear`, `/compact`, `/skill:<name>`, `/quit`) behaves as the
  matching control request, exactly like the REPL; any other line,
  including an unknown `/foo`, is a task for the model.
- `approval_response` `{"approval_id":"appr-1","approved":true}` →
  `{"ok":true}`. The approval id comes from the `approval_request`
  event; `id` (the request's own correlation id) is separate.
- `abort` → `{"aborted":true|false}`. Cancels the running turn, and
  fails any approval waiting on a dialog, so a turn blocked on
  approval can still be stopped.
- `clear_queue` → `{"cleared":N}`. Drops queued prompts, keeps
  control items. Send it before `abort` (Pi's Escape recipe):
  `abort` alone lets queued prompts continue after the turn ends.
- `get_state` → session/model/api/mode/root, `streaming`, `queued`,
  `messages`, `turns`, `total_tokens`.
- `usage` → session usage object, `cost_usd` (null when no rates are
  configured), `fee` (`<Provider> 實價未驗證`), `turns`.
- `jobs` → background bash jobs (id, status, elapsed seconds,
  command, exit code once done).
- `new_session` → `{"session":"<new id>"}`. Starts a fresh session
  and zeroes the runner totals. Session ids are second-resolution
  timestamps; two created in the same second get a `-1`, `-2`…
  suffix, so a new session never reopens the old file.
- `clear` / `compact` → `{"ok":true}` (same as the REPL commands).
- `skill` `{"name":"..."}` → `{"loaded":true,"bytes":N}`.
- `attach_image` `{"path":"..."}` → `{"attached":true,"path":"...","media_type":"...","bytes":N,"width":W,"height":H}`. Queues the image for the next turn, like `/image` in the REPL (same caps, same model-input check).
- `set_model` `{"provider":"...","model":"..."}` (either may be omitted: no provider keeps the current one, no model takes the provider default; a bare `model` searches the current provider first, then the rest) → `{"provider":"...","model":"...","api":"..."}`. Same switch as `/model` in the REPL; `get_state` reports the new model and api at once.
- `quit` → `{"ok":true}`, then the process exits 0. It waits for the
  current turn; send `abort` first to quit at once. Closing stdin
  shuts down the same way (orderly, like Pi's Stop).

Events (stdout):
- `turn_started` `{"text":"<first 200 runes of the task>"}`.
- `text_delta` `{"text":"..."}` — streamed assistant text.
- `step_usage` `{"usage":{"prompt":N,"cached":N,"completion":N,"total":N}}` — per LLM call.
- `tool_start` `{"tool":"read","args":{...}}` — args as parsed JSON
  (the raw string when unparseable).
- `tool_end` `{"tool":"...","output":"...","truncated":bool,"error":null|"..."}` — output capped at 32 KiB head+tail, like the loop's
  truncation for the LLM view.
- `tool_backgrounded` / `background_wait` / `tool_background_done` —
  the background bash events, the same shapes as the CLI's scrollback
  notices.
- `approval_request` `{"id":"appr-1","tool":"write","args":{...},"prompt":"Run write ...?"}` — emitted by the approval bridge (not
  by the loop's `approval` event, which the server ignores to avoid
  sending it twice). The agent blocks until the GUI answers; in
  `-mode auto` or with `--deny-instead-of-prompt` no request is ever
  sent.
- `agent_settled` — exactly one per turn, whatever happened:
  `{"text":"...","usage":{...},"session_usage":{...},"cost_usd":null|N,"session_cost_usd":null|N,"fee":"...","aborted":false,"error":null|"..."}`.
  `aborted` is true only when the GUI aborted the turn; any other
  failure is in `error`. The GUI waits for this event, never for the
  `prompt` response.
- `error` `{"error":"..."}` — protocol-level errors only (a malformed
  line, an unknown request type). Turn errors go through
  `agent_settled`.
