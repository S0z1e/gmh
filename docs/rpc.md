# RPC mode (JSONL over stdio, for a GUI)

`harness --rpc` is the machine frontend: one JSON object per line on
stdin (requests) and stdout (responses and events), LF terminated,
split on LF only, so a GUI
written against this protocol needs only small changes. stdout
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
  `/clear`, `/compact`, `/skill:<name>`, `/sessions`, `/resume <id>`,
  `/rename <old> <new>`, `/plugins`, `/quit`) behaves as the
  matching control request, exactly like the REPL; any other line,
  including an unknown `/foo`, is a task for the model.
- `approval_response` `{"approval_id":"appr-1","approved":true}` →
  `{"ok":true}`. The approval id comes from the `approval_request`
  event; `id` (the request's own correlation id) is separate.
- `continuation_response` `{"continuation_id":"cont-1","decision":"once"|"session"|"deny","updated_args":"{...}"}` →
  `{"ok":true}`. Answers the doom-loop guard's `continuation_request`
  (see below): the model asked for the exact same tool call five
  times in a row. `decision` is `"once"` (run it this once, then the
  streak restarts), `"session"` (never ask again for this exact call
  in this session) or `"deny"` (end the turn). `updated_args` is
  optional: the GUI may let the user edit the repeated call's
  arguments, as a JSON string. This channel is deliberately separate
  from `approval_response`: the question is about the shape of the
  conversation, not whether a tool may act, so no approval rule
  applies to it. In a headless run (`-p`, or no GUI) there is nobody
  to ask and the guard denies.
- `abort` → `{"aborted":true|false}`. Cancels the running turn, and
  fails any approval, continuation, or question waiting on a dialog,
  so a turn blocked on approval can still be stopped.
- `question_response` `{"question_id":"quest-1","answers":[{"question":0,"selected":["Postgres"],"custom":""}]}` →
  `{"ok":true}`. Answers the model's `question_request` (see below):
  one answer per question, `selected` is the chosen option labels
  (several for a multi-select question), `custom` is free text when
  the user answered in their own words instead of picking an option.
  Answering fails a turn blocked on a question the same way `abort`
  does when the turn is aborted.
- `clear_queue` → `{"cleared":N}`. Drops queued prompts, keeps
  control items. Send it before `abort`:
  `abort` alone lets queued prompts continue after the turn ends.
- `get_state` → session/model/api/mode/root, `tool_mode`
  (`"default"` or `"plan"`), `streaming`, `queued`, `messages`,
  `turns`, `total_tokens`, `context_window` and `max_tokens` (the
  effective window and output cap for the current model — the values
  Setup resolved, so a GUI can meter context against the limit the
  compact trigger uses).
- `usage` → session usage object, `cost_usd` (null when no rates are
  configured), `fee` (a note that the price is unverified against the
  provider's actual prices), `turns`.
- `jobs` → background bash jobs (id, status, elapsed seconds,
  command, exit code once done).
- `new_session` → `{"session":"<new id>"}`. Starts a fresh session
  and zeroes the runner totals. Session ids are second-resolution
  timestamps; two created in the same second get a `-1`, `-2`…
  suffix, so a new session never reopens the old file.
- `clear` / `compact` → `{"ok":true}` (same as the REPL commands).
- `rollback` `{"count":N}` → `{"rolled_back":N,"dropped":M,"messages":K,"archive":"..."}`. Drops the last N turns (default 1) from the session — Codex's `thread/rollback`, the GUI's undo. A turn is one user task plus its replies; the pre-rollback transcript is archived first (like compaction), then the session file is rewritten to the kept prefix. Rolling back more turns than exist drops everything; rolling back an empty session is an error. Session usage totals are not rewound.
- `skill` `{"name":"..."}` → `{"loaded":true,"bytes":N}`.
- `attach_image` `{"path":"..."}` → `{"attached":true,"path":"...","media_type":"...","bytes":N,"width":W,"height":H}`. Queues the image for the next turn, like `/image` in the REPL (same caps, same model-input check).
- `set_model` `{"provider":"...","model":"..."}` (either may be omitted: no provider keeps the current one, no model takes the provider default; a bare `model` searches the current provider first, then the rest) → `{"provider":"...","model":"...","api":"..."}`. Same switch as `/model` in the REPL; `get_state` reports the new model and api at once.
- `list_sessions` → `{"sessions":[{"id":"...","messages":N,"modified":"...","preview":"...","current":true}]}` (newest first; audit sidecars and pre-compact archives are not sessions).
- `list_plugins` → `{"plugins":[{"name":"...","version":"...","description":"...","tools":["..."],"state":"loaded"|"disabled"|"skipped","error":"..."}]}` (directory order; `error` only for a skipped plugin, with the load failure as the reason; a disabled plugin's tools list is empty).
- `list_models` → `{"providers":[{"name":"...","api":"...","models":[{"id":"...","name":"...","api":"...","context_window":N,"max_tokens":N,"input":["text"]}]}]}` (providers sorted by name; the resolved values Setup computes — per-model overrides applied, defaults filled — not the raw config file; a provider's flat model is listed too when it is not in its models array).
- `list_skills` → `{"skills":[{"name":"...","description":"..."}]}` (the discovered skills, the same listing the system prompt carries).
- `history` → `{"session":"<id>","messages":[...]}`. The session
  transcript in the session-file JSON shape (`role`/`content`/
  `tool_calls`/`tool_call_id`/`images`; image bytes are never in it,
  only path/media type/dimensions). Defaults to the current session;
  `{"session":"<id>"}` fetches another session's transcript (an
  unknown id is an error). The full history, no truncation — the GUI
  asked for it. After a `/clear` or a compact the file only holds
  what the model still sees, so history matches the live context.
- `rename_session` `{"name":"old","new":"new"}` → `{"renamed":true,"from":"old","to":"new"}`. Same rename as `/rename` in the REPL.
- `resume_session` `{"session":"..."}` → `{"session":"...","messages":N}`. Same switch as `/resume` in the REPL; `get_state` reports the new session at once.
- `delete_session` `{"session":"..."}` → `{"deleted":true,"session":"..."}`. Removes another session's transcript, audit sidecar and pre-compact archives. The current session cannot be deleted (resume or start another one first); an unknown id is an error.
- `set_tool_mode` `{"mode":"plan"|"default"}` → `{"tool_mode":"plan"}`. First-class switch of the loop's tool set (read-only plan mode, or back to the full default set): the same effect as the plan plugin's `/plan` / `/execute`, but it works even when the project has no plan plugin. An unknown or missing mode is an error; switching to the current mode is a no-op. `get_state` and `agent_settled` report the new mode at once. The mode is session-only: a new or resumed session resets it to default.
- `command` `{"name":"plan","message":""}` → `{"ok":true,"output":"..."}`. Runs a plugin command by name (`message` is the raw args string, the text after `/name`); same as typing `/name` in the REPL. A `/name ...` prompt line behaves the same way.
- `quit` → `{"ok":true}`, then the process exits 0. It waits for the
  current turn; send `abort` first to quit at once. Closing stdin
  shuts down the same way (orderly).

Events (stdout):
- `turn_started` `{"text":"<first 200 runes of the task>"}`.
- `text_delta` `{"text":"..."}` — streamed assistant text.
- `thinking_delta` `{"text":"..."}` — one streamed thinking
  fragment (the model's reasoning, not the answer). Deltas only, like
  `text_delta`; no `thinking_start` event.
- `thinking_end` `{"text":"..."}` — the full thinking text for one
  LLM call, so the GUI can replace the accumulated deltas with the
  authoritative text. Emitted only when the thinking is non-empty; a
  call with no thinking emits neither event. Thinking is display-only:
  it never enters `agent_settled.text`, the session file, or
  `history`.
- `step_usage` `{"usage":{"prompt":N,"cached":N,"completion":N,"total":N,"reasoning":N}}` — per LLM call. `reasoning` is the thinking
  part of the completion tokens — a subset of `completion`, never an
  extra charge — and is omitted when the provider did not report it.
- `tool_start` `{"tool":"read","args":{...}}` — args as parsed JSON
  (the raw string when unparseable).
- `tool_end` `{"tool":"...","output":"...","truncated":bool,"error":null|"...","detail":"...","count":N}` — output capped at 32 KiB head+tail, like the loop's
  truncation for the LLM view. `detail` carries the write content / edit diff (display-only, never sent to the model); `count` is the
  grep/glob match count (omitted when 0).
- `tool_backgrounded` / `background_wait` / `tool_background_done` —
  the background bash events, the same shapes as the CLI's scrollback
  notices.
- `approval_request` `{"id":"appr-1","tool":"write","args":{...},"prompt":"Run write ...?"}` — emitted by the approval bridge (not
  by the loop's `approval` event, which the server ignores to avoid
  sending it twice). The agent blocks until the GUI answers; in
  `-mode auto` or with `--deny-instead-of-prompt` no request is ever
  sent.
- `continuation_request` `{"id":"cont-1","tool":"read","prompt":"<canonical args>","repeat_count":5,"threshold":5}` — the doom-loop guard: the same tool call (tool name + canonical arguments) was requested five consecutive times. A different file, command or error breaks the streak, and a tool result that differs from the previous one for the same call does too (the call is making progress). The agent blocks until the GUI answers with `continuation_response`.
- `question_request` `{"id":"quest-1","questions":[{"question":"Which database?","header":"DB","options":[{"label":"Postgres","description":"..."}],"multi_select":false}]}` — the model's `ask_user` tool: up to 4 questions, 2–4 options each. The agent blocks until the GUI answers with `question_response`. The answers reach the model as a tool observation; in a headless run there is nobody to ask, so the model gets an observation telling it to proceed with its best judgement and state its assumptions. The TUI does not ask either (same headless observation).
- `stop_blocked` `{"text":"go test failed"}` — a plugin `stop` hook vetoed the turn end (see `docs/plugins.md`). The turn continues; the GUI should show the reason as a notice, not as a failure. After 8 consecutive vetoes the turn is allowed to end anyway.
- `step_retry` `{"text":"LLM call failed (attempt 1/4): LLM API 403 Forbidden: ...; retrying in 2s"}` — a transient LLM failure is being retried; the turn continues. The GUI should show it as a notice, not as a failure.
- `step_discard` `{"text":"LLM stream cut off after partial output (...); discarding the partial reply"}` — the stream was cut off after showing partial text/thinking, and the call is being replayed from scratch. The GUI must drop everything it showed for the current step (text and thinking deltas since the last `step_usage`) before the replay's deltas arrive, or the half-reply will be duplicated.

Transient LLM failures are retried, not fatal: a failed LLM call is
retried up to 4 attempts in total (the first attempt plus up to 3
retries) when the failure is transient — HTTP 403, 429, 500, 502,
503 or 504, a dropped connection, or a stream cut off mid-reply —
with exponential backoff of 2s, 4s, 8s (a provider
`Retry-After` header replaces the backoff, capped at 60s). 401 and
other 4xx are never retried. A cut-off stream is retried too: the
partial reply is discarded first (a `step_discard` event tells the
GUI to drop it), so streamed text is never duplicated. Only a
completed message is never replayed: once the done message has
arrived, a later error ends the turn instead. A turn that still fails reports
`(after 4 attempts)` in the step error.
- `agent_settled` — exactly one per turn, whatever happened:
  `{"text":"...","usage":{...},"session_usage":{...},"cost_usd":null|N,"session_cost_usd":null|N,"fee":"...","aborted":false,"reason":"end_turn"|"max_turns"|"abort"|"error"|"repeated_tool_call_denied"|"blocked","error":null|"...","tool_mode":"default"|"plan"}`.
  `reason` is the structured terminal reason: `"end_turn"` (the model
  finished), `"max_turns"` (step cap), `"abort"` (the GUI aborted the
  turn), `"error"` (LLM/provider failure),
  `"repeated_tool_call_denied"` (the user denied a repeated call at
  the continuation prompt, or no prompt was available) or `"blocked"`
  (3 consecutive steps whose tool calls all failed — the task was
  marked blocked instead of looping). `aborted` is
  true only when the GUI aborted the turn — and an aborted turn
  carries no `error` string: cancellation is semantic, not an error.
  Any other failure is in `error`. `text` is the turn's final answer
  even when the turn ended in error: on `"max_turns"` it is the
  step-cap grace summary, on `"error"` or `"blocked"` the model's
  last message — empty only when the turn produced no text. The GUI waits for this event, never for the
  `prompt` response. `tool_mode` is the loop's tool mode at the end
  of the turn, so the GUI knows which tools the next turn offers.
- `compacted` `{"messages":N,"summary_tokens":N}` — a compaction
  succeeded (manual `/compact` or automatic after a turn): the
  history was replaced by a summary, `messages` is the new message
  count, `summary_tokens` is the summary's token count (the
  completion tokens of the compact call, which generated it). The
  CLI and TUI keep their `[compact]` print and see nothing new; the
  GUI should re-fetch `history` after this event, since the
  transcript it holds is now stale.
- `error` `{"error":"..."}` — protocol-level errors only (a malformed
  line, an unknown request type). Turn errors go through
  `agent_settled`.
