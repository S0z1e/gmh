# minimal-harness v1

A minimal, Pi-light coding agent harness in Go. Stdlib only, zero dependencies.
OpenAI-compatible chat completions + SSE streaming. Six tools: read, write, edit, bash, grep, glob.
Usage is shown per turn and per session (prompt, cached, completion, total tokens); no fee is estimated — OpenConsole's real price is unverified.

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
- Modes: `-mode suggest` (default, ask before write/edit/bash), `-mode auto` (no prompts, use only on a disposable copy).
- Skills: put `SKILL.md` files under `.agents/skills/<name>/` in the project, or `~/.config/minimal-harness/skills/`. Only name + description are loaded at startup; the full file loads on demand via `/skill:<name>`.
- Sessions live in `~/.minimal-harness/sessions/<id>.jsonl`.
- REPL commands: `/usage` shows session token totals, `/clear` drops the conversation context (a marker is written to the session file; `--resume` starts from the clear point, older messages stay in the file).
- Compaction: `/compact` summarises the conversation into a 6-section brief and replaces history with it; the original transcript is saved to `<session>-pre-compact-<time>.jsonl`. It also triggers automatically when the last call's context reaches 70% of the window (`-context-limit`, default 1048576 for deepseek-v4.1-flash).
- Events: the agent loop emits an event stream (`text_delta`, `step_usage`, `tool_start`, `tool_end`, `done`, `error`); the CLI is one renderer of it, a future TUI will be another. The loop never imports UI code.
- TUI (Bubble Tea v2, inline-first): a separate binary so `-p` / the plain CLI never import it:
  ```
  go run ./cmd/harness-tui -root /path/to/project
  ```
  Finished turns are committed to scrollback with `tea.Println`; only the live region (streaming text, current tool, input line) repaints. The agent loop runs in a goroutine and feeds events into the program. In `-mode suggest`, a write/edit/bash call opens a y/n approval dialog that replaces the input line (single key, no Enter); the agent blocks on a reply channel and never touches Bubble Tea. Bubble Tea v2.1.1 is confined to `internal/tui` + `cmd/harness-tui`; `internal/agent`, `internal/llm`, `internal/tools`, `internal/session`, `internal/skills`, `internal/app` and `cmd/harness` stay stdlib-only (verified with `go list -deps`).
