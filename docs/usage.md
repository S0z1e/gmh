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
