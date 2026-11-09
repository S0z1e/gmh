# minimal-harness v1

A minimal, Pi-light coding agent harness in Go. Stdlib only, zero dependencies.
OpenAI-compatible chat completions + SSE streaming. Six tools: read, write, edit, bash, grep, glob.
Usage is shown per turn and per session (prompt, cached, completion, total tokens). Token counts always come from the API's returned usage — the harness never estimates tokens locally (no local tokenizer; tiktoken would undercount non-OpenAI models by ~15–20%). Cost is estimated only when you set rates: `OPENAI_PRICE_INPUT_PER_MTOK`, `OPENAI_PRICE_OUTPUT_PER_MTOK`, `OPENAI_PRICE_CACHED_PER_MTOK` (USD per 1M tokens), plus `_PEAK_` variants and `OPENAI_PRICE_SOURCE` to label where the rates came from. Peak hours are Mon–Fri 01:00–04:00 and 06:00–10:00 UTC; without peak rates the off-peak rates apply. Every estimate is labelled with its source and "OpenConsole 實價未驗證"; without rates, no estimate is shown.

## Safety (suggest mode) — what it can and cannot promise

Approval is a policy boundary, **not a sandbox**. In `-mode suggest`, write/edit/bash ask first (stdin in plain mode, a y/n dialog in the TUI); read, grep, glob and `load_skill` never ask. `-p` print mode and `--deny-instead-of-prompt` (also `OPENAI_DENY_INSTEAD_OF_PROMPT=1`) refuse every approval without asking — they only ever deny, never approve, and the turn continues with the denial as an observation.

What is enforced:
- read/write/edit/grep/glob paths are jailed to the workspace root (`..` escapes refused); bash starts in the root.
- bash has a timeout (default 60s, per-call up to 300s, `-bash-timeout` / `OPENAI_BASH_TIMEOUT_SECONDS`) and a 64 KiB output cap.
- A denylist refuses the obvious catastrophes before they run: `rm -rf /` (or `~`), `mkfs` with a device, `dd` to a device, writes into `~/.ssh` / `~/.aws` / `~/.config/gcloud` / `~/.kube` / `~/.docker`, and anything touching the Docker socket.
- Every approval decision, every bash run (with exit code), and every denial is written to `<session>-audit.jsonl` next to the session transcript, for later review.

## Background bash (long commands don't block the loop)

A bash call that runs longer than the background threshold (default 10s, `-bg-threshold` / `OPENAI_BG_THRESHOLD_SECONDS`; `0` disables it) keeps running as a job instead of blocking the model: the model gets a "still running in the background (job bg-1)" observation and continues with other work. When the model stops calling tools, the loop waits for the running jobs and feeds each result back as a user message ("Background job bg-1 finished (exit 0). Output: …"), so the model can act on it. The result is a user message, not a second tool message for the same call id — the call already has its tool result, and a second one with the same id is a 400 from the API (found live). A bash call that finishes within the threshold never becomes a job. `/jobs` lists the background jobs (id, status, elapsed, command, exit code once done). The denylist, timeout, 64 KiB output cap and audit log apply to background jobs exactly as to synchronous ones. `-p` print mode never backgrounds (it stays synchronous). The TUI shows the same events as scrollback notices ("… bash running in background (bg-1)", "waiting for background jobs: bg-1", "background job bg-1 finished"). Verified live 2026-11-09: `sleep 12; echo SLOW-DONE` backgrounded at 10s while the model read a file and wrote another, then reported `SLOW-DONE` after the result was fed back. If the step cap hits (or the API call fails) with jobs still running, their results are still delivered to the session before the error is reported, so the work is not silently lost; a fed-back result is capped at 32 KiB head+tail, like a recent tool result. Quitting with jobs running warns that they are abandoned (their child processes keep running, orphaned).

What is NOT promised (residual risk — run on disposable copies or in a container):
- The denylist is pattern matching over the command text; it can be bypassed with variables, quoting, or indirection.
- Bash is only started in the root — once a bash call is approved it can `cd` out and write anywhere, including paths the write/edit tools refuse. Denying write/edit does not stop bash from writing.
- There is no OS sandbox in v0.2 (no bwrap / sandbox-exec); the denylist and jail are harm reduction, not isolation.

## Layout

```
cmd/harness/main.go      // CLI: -p print mode, REPL, --resume, -mode suggest/auto
internal/llm/client.go   // OpenAI-compatible client, streaming, tool-call accumulation
internal/agent/loop.go   // single loop: LLM -> tool -> observe -> repeat
internal/tools/tools.go  // read/write/edit/bash, workspace-root jail, timeout
internal/tools/grep.go   // grep tool (ripgrep backend, grep -r fallback, match cap + truncation)
internal/tools/glob.go   // glob tool (rg --files backend, WalkDir fallback, limit + truncation)
internal/session/session.go // JSONL append + resume
internal/skills/skills.go  // .agents/skills/*/SKILL.md discovery, listing only
internal/rpc/server.go     // RPC mode: JSONL over stdio for a GUI (see below)
internal/rpc/protocol.go   // RPC request/response/event JSON types
internal/agent/prompts/system.md // lightweight system prompt, embedded
```

## Run

```bash
cd minimal-harness
export OPENAI_API_KEY="..."
export OPENAI_BASE_URL="https://www.openconsole.cn/v1"
export OPENAI_MODEL="deepseek-v4.1-flash"
export OPENAI_API_MODE="chat"   # chat (default), responses or anthropic

# print mode (headless, v1)
go run ./cmd/harness -p -root /path/to/project "look at this project and add a test"

# Responses API instead of Chat Completions
go run ./cmd/harness -api responses -p -root /path/to/project "look at this project and add a test"

# Anthropic Messages API (e.g. Discovery's Anthropic route)
go run ./cmd/harness -api anthropic -p -root /path/to/project "look at this project and add a test"

# interactive REPL
go run ./cmd/harness -root /path/to/project

# resume a session
go run ./cmd/harness --resume <session-id> -root /path/to/project
```

Notes:
- The API key is read only from the environment. Never commit it, never put it in a file.
- `OPENAI_BASE_URL` may be given with or without a trailing `/v1`; the client normalises it.
- `-api chat` uses `POST /v1/chat/completions` (messages, `tool_calls[]`); `-api responses` uses `POST /v1/responses` (instructions + input items, `function_call` / `function_call_output`, `store: false` — stateless, JSONL is the store). Not every provider/model supports Responses; if OpenConsole rejects it, the error is returned verbatim.
- `-api anthropic` uses `POST /v1/messages` (system string + messages with `tool_use` / `tool_result` content blocks). The same base URL works: `.../v1` normalises to `.../v1/messages`. Auth is picked by host: `api.anthropic.com` gets `x-api-key` only; every other host (including Discovery) gets Bearer only — sending both breaks Discovery, which prefers `x-api-key` and then rejects the unexchanged surrogate (verified live 2026-11-09). History is translated at call time, so sessions stay in the shared format and replay across modes; consecutive same-role messages merge into one (Anthropic requires strict user/assistant alternation), and an empty tool result replays as `(no output)`. Thinking blocks are skipped like `reasoning_content` in chat mode. Anthropic requires `max_tokens` per response: default 8192, or `ANTHROPIC_MAX_TOKENS`. Verified live 2026-11-09 against Discovery `Agents-A1`: 4-turn REPL (read → write, read-back → write, empty-output bash, follow-up question answered from history) plus a `--resume` run that answered from the earlier calls — tool_use/tool_result pairing and the empty-result placeholder both replay cleanly. The fee label names the provider from the base URL (`Discovery 實價未驗證` on Discovery, `OpenConsole 實價未驗證` on OpenConsole) instead of hardcoding OpenConsole.
- End-to-end with the vault credential: `scripts/run_with_vault_key.py` fetches an `hsurr:` surrogate from authd and passes it to the harness as `OPENAI_API_KEY` in the child process only, never printing or storing it. Verified 2026-11-09: chat mode listed, wrote and verified `hello.txt` (6 bytes, usage total 4655); responses mode read, wrote and verified `count.txt` (usage total 3591). Both sessions are in `~/.minimal-harness/sessions/`.
- Responses API live compatibility, verified 2026-11-09 against OpenConsole `deepseek-v4.1-flash` (P2-7): multi-turn tool loop (read → write → read-back, `summary.txt` exact), fragmented tool-call arguments (raw SSE showed one call split into 8 tiny deltas; a 15-line `write` call landed correctly), two tool calls in one turn, prompt caching (`input_tokens_details.cached_tokens` present — 128 on a replayed call, 6528 in one harness turn — and the cost line prints in responses mode), session resume with replayed `function_call` + `function_call_output` pairs (the model answered from the earlier calls, nothing dropped), and error shapes (bad model → HTTP 403 `{"error":{"message":"当前模型不可用…","code":"model_unavailable"}}`, surfaced verbatim as `LLM API 403 Forbidden: …`). No reasoning events are sent by this model, so there is nothing to surface or drop. No code fix was needed; the regression tests added in `internal/llm/responses_test.go` lock in the verified behaviour (two calls in one turn, denied-call replay pairing, cached-token mapping).
- Stream integrity: a chat stream that ends without `[DONE]`, a Responses stream without `response.completed`, or an Anthropic stream without `message_stop`, is an error, not a silently truncated reply. (OpenConsole's Responses stream sends `response.completed` and no `[DONE]` — verified live 2026-11-09.) A Responses `response.completed` that lists only some of the calls the stream carried no longer drops the rest: the missing ones are merged from the accumulated fragments. Chat streams request `stream_options: {"include_usage": true}` — without it a spec-compliant provider sends no usage object at all and the turn reports 0 tokens (found live 2026-11-09: Discovery's chat stream carried no `usage` until the flag was added; OpenConsole/deepseek includes it by default, which had masked the gap).
- Modes: `-mode suggest` (default, ask before write/edit/bash), `-mode auto` (no prompts, use only on a disposable copy).
- Skills: put `SKILL.md` files under `.agents/skills/<name>/` in the project, or `~/.config/minimal-harness/skills/`. Only name + description are loaded at startup; the full text loads on demand three ways: the user types `/skill:<name>`, the task names a skill as a whole word (auto-loaded before the turn), or the model calls the `load_skill` tool when a task matches a skill. A skill's `references/` files are never loaded up front: `load_skill` lists them in a footer, and the model loads one with `load_skill {name, reference}` (paths are jailed to that skill's `references/`).
- Sessions live in `~/.minimal-harness/sessions/<id>.jsonl`.
- REPL commands: `/usage` shows session token totals, `/clear` drops the conversation context (a marker is written to the session file; `--resume` starts from the clear point, older messages stay in the file).
- Compaction: `/compact` summarises the conversation into a 6-section brief and replaces history with it; the original transcript is saved to `<session>-pre-compact-<time>.jsonl`. It also triggers automatically when the last call's context reaches 70% of the window (`-context-limit`, default 1048576 for deepseek-v4.1-flash).
- Events: the agent loop emits an event stream (`text_delta`, `step_usage`, `tool_start`, `tool_end`, `done`, `error`); the CLI is one renderer of it, the TUI and the RPC server are two more. The loop never imports UI code.
- UI selection (`--ui auto|plain|tui`, default `auto`): precedence is flag > `NO_COLOR` (non-empty) > `TERM=dumb` > TTY. On a terminal, `auto` opens the TUI; piped, `NO_COLOR`, or `TERM=dumb` means plain. `-p` is always plain and ignores `--ui`. `harness --ui tui` without a terminal falls back to plain with a warning. The TUI is a separate binary (`harness-tui` next to `harness`); `harness` re-execs it when the TUI is selected, so `-p` / the plain CLI never import Bubble Tea.
- TUI (Bubble Tea v2, inline-first): a separate binary so `-p` / the plain CLI never import it:
  ```
  go run ./cmd/harness-tui -root /path/to/project
  ```
  Finished turns are committed to scrollback with `tea.Println`; only the live region (streaming text, current tool, input line) repaints. The agent loop runs in a goroutine and feeds events into the program. In `-mode suggest`, a write/edit/bash call opens a y/n approval dialog that replaces the input line (single key, no Enter); the agent blocks on a reply channel and never touches Bubble Tea. Bubble Tea v2.1.1 is confined to `internal/tui` + `cmd/harness-tui`; `internal/agent`, `internal/llm`, `internal/tools`, `internal/session`, `internal/skills`, `internal/app` and `cmd/harness` stay stdlib-only (verified with `go list -deps`).
- TUI look (opencode-style restyle 2026-11-09, lipgloss v2 — also confined to `internal/tui`; no new dependencies, the markdown renderer is hand-rolled stdlib): dark palette (`#0a0a0a` bg, `#141414` raised, `#484848` border, `#eeeeee` text, `#808080` muted, peach `#fab283` and purple `#9d7cd8` accents). No top header bar in the live region — the header (`▪ harness · <api> · <model>` + dim root/session/mode line) is printed once at startup into scrollback. User turns are raised panels with a peach `┃` left border; assistant text is markdown-rendered (bold, code, quotes, lists, fenced blocks) and indented 3 spaces; tool rows are muted (`✓ Read t.txt`, `✗ Write u.txt` + collapsed error), bash shows a raised block with the command in the top border (`┤ $ <cmd> ├`, btop-style) + output tail (≤10 lines); while working, a braille spinner (80 ms) shows the pending tool label in plain words with its object (`Reading src/foo.ts…`, `Preparing write u.txt…`, `Searching "TODO" in .…`) or, when no tool runs, `Thinking…` — but only after the turn has run 300 ms with no text (Claude Code waits 2 s; ours is shorter), so short turns never flash a spinner at all; usage lines stay in the timeline (opencode hides them in a `/usage` dialog — ours keeps them, restyled with thousands separators). The approval dialog is a full-width panel with a peach `┃` left border (not a bordered box): `△ Approval required`, the tool summary, the raw request, and `[y] Allow once` on a solid blue background with `[n] Deny` and `esc = deny` (single key, Esc = deny). The composer is a `┃` input line with placeholder `Ask anything… /skill:<name> /usage /jobs /clear /compact /quit` and a meta line split left/right like a statusline: left is the mode as a two-colour chip (dark text on peach, yazi-style) · model · api, right is the workspace git branch and the session totals (`3 turns · 12,345 tokens`). Themes are tokenised in `internal/tui/theme.go` (`Theme` struct + `SetTheme`; `HARNESS_TUI_THEME=default|terminal|catppuccin-mocha` picks one): `terminal` paints no raised background so the user's own terminal background shows through, `catppuccin-mocha` is the verified Catppuccin Mocha palette. The TUI stays inline (alt-screen was rejected: it would give up shell scrollback and force a rewrite of the viewport/composer/focus logic). A finished turn's reply is held as `pendingReply` and printed together with the runner's usage lines in ONE `tea.Println`: two separate Println commands can execute out of order (each command runs in its own goroutine), which once put the usage above the reply. The live text is a plain `string`, not a `strings.Builder` — a Builder field panics ("illegal use of non-zero Builder copied by value") on the second streamed chunk, because Bubble Tea copies the model by value on every Update; a real multi-chunk reply killed the program mid-turn before this was fixed (regression test `TestModelStreamsManyDeltas`). Screenshots (real tmux captures): `harness-tui-screenshot.png` (real model, Discovery Agents-A1), `harness-tui-approval.png` (approval dialog, mock-driven). The earlier `Thinking…` flash after a turn ends (a line visible for under a second, then gone) is fixed by the 300 ms spinner delay for the fast turns where it was observed: the model state was always correct (unit-tested) and the flash was a renderer repaint race — a 60 fps flush can paint a mid-turn frame after the turn-end prints land, and the next flush clears it. A slow turn can still, in principle, show one stale frame for ~16 ms; that is a Bubble Tea repaint timing, not a state bug.

## RPC mode (JSONL over stdio, for a GUI)

`harness --rpc` is the machine frontend: one JSON object per line on stdin (requests) and stdout (responses and events), LF terminated, split on LF only — the same framing as `pi --mode rpc`, so a GUI written against Pi's protocol needs only small changes. stdout carries protocol data only; diagnostics go to stderr. `-p` conflicts with `--rpc`; `--ui` is ignored.

```bash
harness --rpc -root /path/to/project -mode suggest
```

Startup: the first line on stdout is a `ready` event: `{"type":"ready","protocol":1,"session":"<id>","model":"...","api":"chat","mode":"suggest","root":"...","resumed":false,"messages":0}`.

Requests (each `{"id":"<req>","type":"..."}`; `id` is optional and echoed in the response):
- `prompt` `{"message":"..."}` → `{"disposition":"started"|"queued"}`. The response only means accepted; the turn ends with `agent_settled`. A second prompt while a turn runs is queued (cap 64; beyond that an error). A known slash line (`/usage`, `/jobs`, `/clear`, `/compact`, `/skill:<name>`, `/quit`) behaves as the matching control request, exactly like the REPL; any other line, including an unknown `/foo`, is a task for the model.
- `approval_response` `{"approval_id":"appr-1","approved":true}` → `{"ok":true}`. The approval id comes from the `approval_request` event; `id` (the request's own correlation id) is separate.
- `abort` → `{"aborted":true|false}`. Cancels the running turn, and fails any approval waiting on a dialog, so a turn blocked on approval can still be stopped.
- `clear_queue` → `{"cleared":N}`. Drops queued prompts, keeps control items. Send it before `abort` (Pi's Escape recipe): `abort` alone lets queued prompts continue after the turn ends.
- `get_state` → session/model/api/mode/root, `streaming`, `queued`, `messages`, `turns`, `total_tokens`.
- `usage` → session usage object, `cost_usd` (null when no rates are configured), `fee` (`<Provider> 實價未驗證`), `turns`.
- `jobs` → background bash jobs (id, status, elapsed seconds, command, exit code once done).
- `new_session` → `{"session":"<new id>"}`. Starts a fresh session and zeroes the runner totals. Session ids are second-resolution timestamps; two created in the same second get a `-1`, `-2`… suffix, so a new session never reopens the old file.
- `clear` / `compact` → `{"ok":true}` (same as the REPL commands).
- `skill` `{"name":"..."}` → `{"loaded":true,"bytes":N}`.
- `quit` → `{"ok":true}`, then the process exits 0. It waits for the current turn; send `abort` first to quit at once. Closing stdin shuts down the same way (orderly, like Pi's Stop).

Events (stdout):
- `turn_started` `{"text":"<first 200 runes of the task>"}`.
- `text_delta` `{"text":"..."}` — streamed assistant text.
- `step_usage` `{"usage":{"prompt":N,"cached":N,"completion":N,"total":N}}` — per LLM call.
- `tool_start` `{"tool":"read","args":{...}}` — args as parsed JSON (the raw string when unparseable).
- `tool_end` `{"tool":"...","output":"...","truncated":bool,"error":null|"..."}` — output capped at 32 KiB head+tail, like the loop's truncation for the LLM view.
- `tool_backgrounded` / `background_wait` / `tool_background_done` — the background bash events, the same shapes as the CLI's scrollback notices.
- `approval_request` `{"id":"appr-1","tool":"write","args":{...},"prompt":"Run write ...?"}` — emitted by the approval bridge (not by the loop's `approval` event, which the server ignores to avoid sending it twice). The agent blocks until the GUI answers; in `-mode auto` or with `--deny-instead-of-prompt` no request is ever sent.
- `agent_settled` — exactly one per turn, whatever happened: `{"text":"...","usage":{...},"session_usage":{...},"cost_usd":null|N,"session_cost_usd":null|N,"fee":"...","aborted":false,"error":null|"..."}`. `aborted` is true only when the GUI aborted the turn; any other failure is in `error`. The GUI waits for this event, never for the `prompt` response.
- `error` `{"error":"..."}` — protocol-level errors only (a malformed line, an unknown request type). Turn errors go through `agent_settled`.

Verified 2026-11-09: unit tests plus a fake-LLM integration test in `internal/rpc/server_test.go` (approval approve/deny, abort mid-flight, queued prompts, new_session, malformed input); a scripted Python client against the mock OpenAI server (`/tmp/tui-shot/rpc_client.py`: full suggest-mode flow — read, approval, write, usage, get_state, new_session, quit — plus the abort flow with a 3 s delayed mock); and a live run against Discovery Agents-A1 (`/tmp/tui-shot/rpc_client_real.py`: read → write with the approval answered over the protocol, real usage, `w.txt` verified).

A same-second session-id collision was found by the RPC tests and fixed in `internal/session/session.go`: auto-generated ids get a `-1`, `-2`… suffix when the file already exists (an explicit id, e.g. `--resume`, is never suffixed). The same bug could previously merge two CLI processes started in the same second into one session file.
