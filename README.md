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
internal/agent/prompts/system.md // lightweight system prompt, embedded
```

## Run

```bash
cd minimal-harness
export OPENAI_API_KEY="..."
export OPENAI_BASE_URL="https://www.openconsole.cn/v1"
export OPENAI_MODEL="deepseek-v4.1-flash"
export OPENAI_API_MODE="chat"   # chat (default) or responses

# print mode (headless, v1)
go run ./cmd/harness -p -root /path/to/project "look at this project and add a test"

# Responses API instead of Chat Completions
go run ./cmd/harness -api responses -p -root /path/to/project "look at this project and add a test"

# interactive REPL
go run ./cmd/harness -root /path/to/project

# resume a session
go run ./cmd/harness --resume <session-id> -root /path/to/project
```

Notes:
- The API key is read only from the environment. Never commit it, never put it in a file.
- `OPENAI_BASE_URL` may be given with or without a trailing `/v1`; the client normalises it.
- `-api chat` uses `POST /v1/chat/completions` (messages, `tool_calls[]`); `-api responses` uses `POST /v1/responses` (instructions + input items, `function_call` / `function_call_output`, `store: false` — stateless, JSONL is the store). Not every provider/model supports Responses; if OpenConsole rejects it, the error is returned verbatim.
- End-to-end with the vault credential: `scripts/run_with_vault_key.py` fetches an `hsurr:` surrogate from authd and passes it to the harness as `OPENAI_API_KEY` in the child process only, never printing or storing it. Verified 2026-11-09: chat mode listed, wrote and verified `hello.txt` (6 bytes, usage total 4655); responses mode read, wrote and verified `count.txt` (usage total 3591). Both sessions are in `~/.minimal-harness/sessions/`.
- Responses API live compatibility, verified 2026-11-09 against OpenConsole `deepseek-v4.1-flash` (P2-7): multi-turn tool loop (read → write → read-back, `summary.txt` exact), fragmented tool-call arguments (raw SSE showed one call split into 8 tiny deltas; a 15-line `write` call landed correctly), two tool calls in one turn, prompt caching (`input_tokens_details.cached_tokens` present — 128 on a replayed call, 6528 in one harness turn — and the cost line prints in responses mode), session resume with replayed `function_call` + `function_call_output` pairs (the model answered from the earlier calls, nothing dropped), and error shapes (bad model → HTTP 403 `{"error":{"message":"当前模型不可用…","code":"model_unavailable"}}`, surfaced verbatim as `LLM API 403 Forbidden: …`). No reasoning events are sent by this model, so there is nothing to surface or drop. No code fix was needed; the regression tests added in `internal/llm/responses_test.go` lock in the verified behaviour (two calls in one turn, denied-call replay pairing, cached-token mapping).
- Stream integrity: a chat stream that ends without `[DONE]`, or a Responses stream without `response.completed`, is an error, not a silently truncated reply. (OpenConsole's Responses stream sends `response.completed` and no `[DONE]` — verified live 2026-11-09.) A Responses `response.completed` that lists only some of the calls the stream carried no longer drops the rest: the missing ones are merged from the accumulated fragments.
- Modes: `-mode suggest` (default, ask before write/edit/bash), `-mode auto` (no prompts, use only on a disposable copy).
- Skills: put `SKILL.md` files under `.agents/skills/<name>/` in the project, or `~/.config/minimal-harness/skills/`. Only name + description are loaded at startup; the full text loads on demand three ways: the user types `/skill:<name>`, the task names a skill as a whole word (auto-loaded before the turn), or the model calls the `load_skill` tool when a task matches a skill. A skill's `references/` files are never loaded up front: `load_skill` lists them in a footer, and the model loads one with `load_skill {name, reference}` (paths are jailed to that skill's `references/`).
- Sessions live in `~/.minimal-harness/sessions/<id>.jsonl`.
- REPL commands: `/usage` shows session token totals, `/clear` drops the conversation context (a marker is written to the session file; `--resume` starts from the clear point, older messages stay in the file).
- Compaction: `/compact` summarises the conversation into a 6-section brief and replaces history with it; the original transcript is saved to `<session>-pre-compact-<time>.jsonl`. It also triggers automatically when the last call's context reaches 70% of the window (`-context-limit`, default 1048576 for deepseek-v4.1-flash).
- Events: the agent loop emits an event stream (`text_delta`, `step_usage`, `tool_start`, `tool_end`, `done`, `error`); the CLI is one renderer of it, a future TUI will be another. The loop never imports UI code.
- UI selection (`--ui auto|plain|tui`, default `auto`): precedence is flag > `NO_COLOR` (non-empty) > `TERM=dumb` > TTY. On a terminal, `auto` opens the TUI; piped, `NO_COLOR`, or `TERM=dumb` means plain. `-p` is always plain and ignores `--ui`. `harness --ui tui` without a terminal falls back to plain with a warning. The TUI is a separate binary (`harness-tui` next to `harness`); `harness` re-execs it when the TUI is selected, so `-p` / the plain CLI never import Bubble Tea.
- TUI (Bubble Tea v2, inline-first): a separate binary so `-p` / the plain CLI never import it:
  ```
  go run ./cmd/harness-tui -root /path/to/project
  ```
  Finished turns are committed to scrollback with `tea.Println`; only the live region (streaming text, current tool, input line) repaints. The agent loop runs in a goroutine and feeds events into the program. In `-mode suggest`, a write/edit/bash call opens a y/n approval dialog that replaces the input line (single key, no Enter); the agent blocks on a reply channel and never touches Bubble Tea. Bubble Tea v2.1.1 is confined to `internal/tui` + `cmd/harness-tui`; `internal/agent`, `internal/llm`, `internal/tools`, `internal/session`, `internal/skills`, `internal/app` and `cmd/harness` stay stdlib-only (verified with `go list -deps`).
- TUI look (restyled 2026-11-09, lipgloss v2 — also confined to `internal/tui`): the header (bold title, dim meta) is printed once at startup into the scrollback, not kept in the live region; the prompt is a green `❯`, the running tool is an amber `▶ <name>…`, usage lines are dim grey, errors are red, background-job notices are amber, and the approval dialog is an amber rounded border with a bold title and green `[y] approve` / red `[n] deny`. Styling is restrained and functional — meta recedes, actions stand out. A finished turn's reply is held as `pendingReply` and printed together with the runner's usage lines in ONE `tea.Println`: two separate Println commands can execute out of order (each command runs in its own goroutine), which once put the usage above the reply. The live text is a plain `string`, not a `strings.Builder` — a Builder field panics ("illegal use of non-zero Builder copied by value") on the second streamed chunk, because Bubble Tea copies the model by value on every Update; a real multi-chunk reply killed the program mid-turn before this was fixed (regression test `TestModelStreamsManyDeltas`). Screenshots (real tmux captures): `harness-tui-screenshot.png`, `harness-tui-approval.png`.
