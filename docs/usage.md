# Run

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
  other host gets Bearer only — sending both breaks a host that
  expects `x-api-key` instead. History is translated at call time, so
  sessions stay in the shared format and replay across modes;
  consecutive same-role messages merge into one (Anthropic requires
  strict user/assistant alternation), an empty tool result replays as
  `(no output)`, and thinking blocks are skipped on replay (they were
  never persisted — thinking is display-only). `max_tokens` defaults to 16384,
  or `ANTHROPIC_MAX_TOKENS`.
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
- Editing: `edit` takes either `oldText` (must match exactly once —
  a failed match names the closest lines, so the call can be fixed
  instead of re-reading the file) or `startLine`/`endLine` (1-based,
  inclusive; `endLine` defaults to `startLine`) to replace those
  lines with `newText`. Line edits require a `read` in the current session and an unchanged
  file since that read. After an edit or external change, read the
  target lines again; stale edits fail before writing. `write` takes
  `overwrite:true` to replace an existing file wholesale (default:
  it fails if the file exists — use `edit` then); prefer `edit` for
  a small change. Run builds and tests bare,
  never piped through `head`/`tail`, so the exit code is visible.
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
- Project context: `AGENTS.md` and `CLAUDE.md` in the workspace root
  (and ancestors up to `$HOME`) are loaded once at startup into the
  system prompt, each capped at 8 KiB, with a git snapshot (branch,
  working-tree status, recent commits) added the same way. Editing a
  context file mid-session needs a restart.
- Sessions live in `~/.minimal-harness/sessions/<id>.jsonl`.
- REPL commands: `/usage` shows session token totals, `/clear` drops
  the conversation context (a marker is written to the session file;
  `--resume` starts from the clear point, older messages stay in the
  file). `/plugins` lists the discovered plugins with their tools
  and state. `/sessions` lists the sessions (newest first, with message
  count and the first user message as a preview; the current one is
  marked `*`); `/resume <id>` switches to one, `/rename <old> <new>`
  renames one (transcript, audit sidecar and pre-compact archives
  follow). `/model` lists the configured models; `/model <provider>/<model>`
  (or a bare model id) switches model mid-session — the client is
  rebuilt from that model's config (api, baseUrl, maxTokens,
  contextWindow, image input) and history replays through the new
  API on the next turn. The switch lasts for the session only; it
  does not rewrite the config file.
- Compaction: `/compact` summarises the conversation into a 6-section
  brief and replaces history with it; the original transcript is
  saved to `<session>-pre-compact-<time>.jsonl`. It also triggers
  inside the tool loop, before the next model call, when measured
  usage plus new content reaches 70% of the window (`-context-limit`, default: the model's `contextWindow`,
  else 128000). Summary requests use the same bounded retry policy as normal
  steps (four attempts by default, with backoff and `Retry-After` support).
  Retries are labelled `Compaction` in `step_retry` events. Failed or truncated
  summaries never replace history. If retries are exhausted, the turn stops
  with an explicit failure report rather than an empty answer.
- Step budget: steps are unlimited by default (like Claude Code,
  Codex, Gemini, Qwen, OpenCode and Pi): a turn runs until the model
  stops calling tools, an error ends it, or the task is blocked
  (3 consecutive all-failed steps). The bounds are auto-compaction
  at 70% of the window plus the doom-loop guard (5 consecutive
  identical calls) and the stop-hook veto cap (8). Pass
  `-max-steps N` to opt into a hard cap; when a cap hits, the model
  gets one final tool-less call (60s) to summarise what was done,
  then the turn ends `max_turns` — the same shape as OpenCode's
  final step. A turn that ends `blocked` gets the same treatment:
  one tool-less call (60s) whose prompt names the blocked reason
  and the last errors, so the settled text is a short summary of
  what was verified, what failed and what is left — a blocked turn
  used to settle with an empty text and no report at all. A failed
  grace call changes nothing in either case.
- Repeat-failure circuit breaker (after Qoder's denial circuit
  breaker, adapted): the doom-loop guard only sees consecutive
  repeats, so a call that keeps failing with other work in between
  never trips it. The breaker counts failures per tool call
  (tool name + canonical arguments) within one turn: the 2nd
  failure warns in the observation, and the 4th identical attempt
  is denied before it runs, as a tool error with reason
  `repeated_tool_failure_denied` — distinct from the doom-loop
  guard's `repeated_tool_call_denied`, which means the continuation
  was refused. A success clears the count, and counts reset every
  turn. A denied call counts as a failed fix attempt for the
  blocked guard, so three consecutive denied-or-failed steps still
  end the turn `blocked`. The breaker never touches the LLM retry
  path (`step_retry`/idle): that counts failed LLM calls, not tool
  calls, and works independently.
- Timeouts: a model entry may set `timeoutSeconds` (total per-request
  cap, default 600) and `idleTimeoutSeconds` (silence cap while
  streaming, default 60). A stream that produces no SSE line for the
  idle period — no text, no thinking delta, not even a heartbeat —
  is treated as stalled: the partial reply is discarded
  (`step_discard`), the call is retried (`step_retry`, same
  4-attempt budget as other transient failures), and the replay is
  identical. The same idle deadline applies before the first byte:
  a server that sends no response headers within the idle period
  is a stall too. A slow-but-alive stream is never disturbed: any
  line resets the idle timer. The total timeout stays a
  non-retryable backstop.
- Background jobs: a bash call running longer than 120s goes to the
  background (job bg-N) and the loop keeps working; finished jobs
  report in at the next step, in completion order, as user messages
  — never only when the turn ends. Pass `background:true` for a
  command that will not exit on its own (dev server, watcher): it
  returns at once with a job id, and its output is delivered the
  same way. An explicit `background:true` job has NO timeout unless
  a per-call `timeout` is given — a dev server must outlive any
  cap. The bash timeout is 120s by default, 600s max
  (per-call `timeout`, `bashTimeoutSeconds`, or
  `OPENAI_BASH_TIMEOUT_SECONDS`); a timeout kills the whole
  process group, so a command's grandchildren die with it. Every
  bash call is a fresh shell starting in the workspace root — `cd`
  does not persist between calls, so a bare `npm run build` in a
  nested project fails with "Missing script: build". Pass `cwd` to
  run in a subdirectory (relative paths resolve against the root),
  or prefix the command with `cd <dir> && `; an invalid cwd is a
  tool error. Every bash result header names the directory the call
  ran in (`cwd: ...`), so a wrong-directory run is visible at once.
  Every bash call writes its full stdout+stderr to a per-call file under
  the session's `bash-output/` directory; the result shows only the
  first and last 30 lines plus the path. Read the file to see the
  middle, or the partial output of a job that is still running. A
  job that never finishes never blocks the turn from ending: after
  a 30s drain the turn ends with the job still running, the turn
  message gives the output file path so you can read its partial
  output, and the result is delivered at the next turn when the
  job finishes. A background job outlives its turn: it keeps
  running after the turn settles (and after quit, orphaned) until
  it exits on its own or its timeout kills it — so a dev server
  started in one turn is still serving in the next. Output files
  cap at 64 MiB: past that the run is killed and the call fails, so
  a spewing command cannot fill the disk.
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

## TUI

Inline (no alt-screen, so shell scrollback still works), opencode-style:
dark palette with peach `#fab283` and purple `#9d7cd8` accents; user
turns are raised panels with a peach `┃` left border; assistant text
is markdown-rendered and indented; tool rows are muted
(`✓ Read t.txt`, `✗ Write u.txt`); bash shows a raised block with the
command in the top border (`┤ $ <cmd> ├`) and an output tail (≤10
lines). Write and edit show what actually landed in a raised block
below the row — the written content for write, a red/green `-`/`+`
diff of the replaced lines for edit (first 20 lines). Grep and glob
report their count on the row (`Grep "TODO" in . (3 matches)`);
the count comes from the tool, never invented. While working, a braille spinner shows the pending tool label
in plain words (`Reading src/foo.ts…`), or `Thinking…` — but only
after the turn has run 300 ms with no text, so short turns never
flash a spinner. When the model streams thinking, the first thinking
delta replaces that row with the accumulating thinking text, dim and
italic, above the live answer; it stays in the scrollback after the
turn, ahead of the reply. Thinking is display-only: it is never part
of the answer, the session file, or the usage totals. Usage lines stay in the timeline with thousands
separators. The approval dialog is a full-width panel:
`△ Approval required`, the tool summary, `[y] Allow once` on a solid
blue background, `[n] Deny`, `esc = deny` (single key, Esc = deny).
The composer is a `┃` input line with a meta line: left is the mode
chip · model · api, right is the workspace git branch and the session
totals (`3 turns · 12,345 tokens`). Themes:
`HARNESS_TUI_THEME=default|terminal|catppuccin-mocha`; `terminal`
paints no raised background, `catppuccin-mocha` is the Catppuccin
Mocha palette.
