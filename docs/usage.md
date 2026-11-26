# Run

- Set a provider's `apiKey` to a literal key or a `$NAME` / `${NAME}`
  environment reference. Global or explicit config may omit it to use
  `OPENAI_API_KEY`; project providers must declare the reference explicitly.
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
  `(no output)`, and Anthropic thinking blocks (including opaque signatures) are
  persisted and replayed in their original order. `max_tokens` defaults to 16384,
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
- Turn status: a model ending its response does not establish that the task
  is complete. A stop hook that keeps vetoing after three feedback rounds stops
  with an explicit incomplete error, preserved for resume. `-p` returns a
  nonzero exit status when the turn fails, including a cap or blocked task.
- Each agent turn defaults to a 1-hour wall deadline, 1,000 requested tool calls,
  2,000,000 tokens, and 8 concurrent background-capable Bash executions across
  current and retained sessions on the executor. Sessions default to 10,000,000
  tokens across all turns. Required foreground verification checks do not consume
  a background-job slot. Configure global, explicit or trusted project settings:

  ```json
  {"turnBudget":{"maxDurationSeconds":3600,"maxToolCalls":1000,"maxConcurrentJobs":8,"maxTokens":2000000,"maxSessionTokens":10000000,"maxCostUSD":0,"strictMetering":false}}
  ```

  Omitted fields use these defaults; an explicit zero disables that cap. An omitted
  policy inherits the global policy; a supplied policy replaces it as a whole.
  Untrusted projects cannot override global budgets. Requested calls count even
  when invalid or refused. A batch that exceeds the remaining tool allowance is
  rejected in full, with durable unexecuted results. A fresh turn gets fresh counters.
  The deadline includes provider requests, tools, retries, background-result waits
  and approval/question waits. Custom prompt callbacks must observe the request's
  `Context`. Detached background jobs retain their existing lifecycle after a stop.
- Dollar caps are opt-in via `maxCostUSD` and `maxSessionCostUSD`.
  `maxSessionTokens` and `maxSessionCostUSD` limit cumulative usage across turns,
  manual compaction and `--resume`.
  Caps use reported usage from provider attempts, including failed retries,
  automatic compaction and grace calls. Requests rejected before inference
  can be retried without a usage report. A final response can use the exact
  allowance, after which another provider attempt is blocked. Usage arrives
  after execution, so an in-flight request can overshoot. Its reported usage
  is retained. Dollar caps use configured rate estimates and require cache
  usage when input and cache-read rates differ. Environment pricing requires
  all three base `OPENAI_PRICE_*_PER_MTOK` rates, including explicit zeros,
  when the model has no configured `cost` schedule. Environment rates override
  the corresponding model rates. Anthropic also requires a cache-write rate.
  By default, missing usage leaves a metering gap. Token and dollar caps still
  stop at the recorded subtotal and label it a lower bound; they do not treat
  that partial sum as a complete bill. An active dollar cap with no price
  schedule at all is not enforced: status, `/usage`, and the turn outcome say
  the cap is not enforced and must not present a zero subtotal as a met cap. A damaged usage journal, torn-tail backup, or unmatched
  pending marker keeps that gap until it is quarantined:
  `harness usage-repair --root <project> --session <id> --quarantine <artifact>`.
  The strict error names the artifact. Quarantine moves it aside and keeps the
  bytes; it does not delete evidence. If you relied on an existing cap stopping whenever usage
  or pricing was incomplete, set `strictMetering: true` in `turnBudget` when
  upgrading. Strict mode stops with `budget_unavailable` when metering required
  by an active cap is incomplete. Exhaustion stops with `budget_exceeded`. Both save an
  `incomplete` task outcome with the budget snapshot and send no extra grace call.
  Manual `/compact` uses the same turn token cap, dollar cap, and strict-metering
  setting as a model turn, plus the session caps. It does not apply the turn's
  wall-clock deadline. Negative, malformed, or overflowing provider usage is
  recorded as unknown. That report does not end a non-strict turn unless an active
  cap needs complete evidence; strict mode stops with `budget_unavailable` before
  another provider attempt. A damaged journal is a different failure and still
  blocks accounting until it is quarantined. Unreported usage is recorded as
  unknown rather than zero. Ordinary UI usage totals remain separate from this
  per-attempt budget evidence.
- Required verification is opt-in in global, explicit or trusted project config:

  ```json
  {"verification":{"required":[{"name":"tests","command":"go test ./...","cwd":"."}],"exclude":["build","node_modules"]}}
  ```

  The model runs these exact commands with the normal Bash tool after its final
  edits, in the foreground and subject to approval, denylist, timeout, sandbox
  and environment rules. A matching check's cwd is made canonical and recorded
  in the final arguments before approval, so an alias changing during snapshot
  capture cannot redirect the approved command. These commands are never
  launched automatically.
  Each result records its invocation, canonical cwd, exit code, start/end times
  and workspace content version in the transcript. A later failed attempt replaces
  an earlier pass; edits after a pass invalidate it. Checks that modify included
  files must run again against the final version. All required checks passing at
  the current version permits `outcome: completed`; missing, failed or stale
  evidence gets up to three feedback rounds, then `verification_failed` and a
  nonzero `-p` exit. Unchanged questions, reads and check-only turns make no
  completion claim or require verification retries.
  Versions cover workspace files, directories and permissions, excluding `.git`
  and explicitly listed relative output paths. Exclude only generated inputs
  that do not need certification. Limits are 50,000 entries, 16 MiB per file and
  128 MiB total content; unsupported files (including symlinks), concurrent
  capture changes or missing directory identity fail closed. External files,
  services, environment changes and the semantic quality of tests are outside
  this proof. An omitted policy inherits global settings; `{"verification":{"required":[]}}`
  disables them when supplied by an authorized config. Untrusted project config
  cannot replace global verification requirements.
- Select an approval mode with `-mode` or JSON `mode`. In the CLI and TUI,
  `/permissions` shows the active mode and `/permissions <mode>` changes
  it for the current process between turns.
  - `ask-for-approval` (default) asks before write, edit, Bash, webfetch
    and plugin calls. In `-p` print mode, those calls are refused.
  - `approve-for-me` reviews each gated call using the active model in a
    separate request without tools. The request includes the original task,
    the user's answers in the current turn, and the complete effective
    invocation, including Bash's resolved working directory. Reviewer usage counts
    toward turn budgets. This mode also works in `-p` print mode.
  - `full-access` skips approval for all tool calls. Tool-level restrictions
    and any configured Bash sandbox still apply.
  - Existing `suggest` and `auto` settings are accepted. `suggest` has
    the same policy as `ask-for-approval`. `auto` skips routine prompts and
    still asks for protected writes and host Git status under `--sandbox`.
- Editing: `edit` takes either `oldText` (must match exactly once —
  a failed match names the closest lines, so the call can be fixed
  instead of re-reading the file) or `startLine`/`endLine` (1-based,
  inclusive; `endLine` defaults to `startLine`) to replace those
  lines with `newText`. A `read` or successful full `write` establishes a known
  version for line edits in the current session. Successful edits update it
  without an extra read only if the original file still matched that version.
  An `oldText` edit can match unread or externally changed content, but does
  not establish a known version in that case: read the target lines before
  a line edit. Stale line edits fail before writing. `write` takes
  `overwrite:true` to replace an existing file wholesale (default:
  it fails if the file exists — use `edit` then); prefer `edit` for
  a small change. Run builds and tests bare,
  never piped through `head`/`tail`, so the exit code is visible.
- Skills: put `SKILL.md` files under `.agents/skills/<name>/` in the
  project, or `~/.config/minimal-harness/skills/`. Only name +
  description are loaded at startup; the full text loads on demand
  when the user types `/skill:<name>` or the model calls the
  `load_skill` tool when a task matches a skill. A skill's
  `references/` files are never loaded up front: `load_skill` lists
  them in a footer, and the model loads one with
  `load_skill {name, reference}` (paths are jailed to that skill's
  `references/`).
- Project guidance: in a trusted workspace, applicable `AGENTS.md` and
  `CLAUDE.md` files are read from the allowed ancestors up to home, the
  workspace root, and directories containing the file being edited. Nearer
  directories refine earlier instructions; `CLAUDE.md` follows `AGENTS.md`
  in the same directory. The model sees each source's path, SHA-256 and
  up to 8 KiB of text (also subject to an overall preview limit). It can
  request the rest with `read_guidance`. If the applicable instructions
  change before a write or edit, the harness asks the model to reconsider
  the operation before executing it. If branch or working tree status is
  needed, `git_status` reads it on demand in trusted workspaces. It requires
  approval in `ask-for-approval`, `suggest` and `approve-for-me`, and in
  legacy `auto` with the Bash sandbox enabled. A linked worktree may keep
  its Git metadata outside that sandbox.
  The result covers the selected working tree, without running Git filters,
  inspecting submodule working trees or reporting upstream tracking status.
  The working tree scan requires Git 2.32 or later. With older or unrecognized
  versions the tool reports only the branch or detached commit.
- Sessions live in `~/.minimal-harness/sessions/<id>.jsonl`. A session has
  one transcript writer across processes; opening, renaming or deleting another
  process's active session fails immediately. Closing or crashing releases the
  lease, including after rollback and rename. Lock files in the `.writers/`
  subdirectory remain in place; do not remove them while harness processes run.
  Writable transcripts must be regular files with one hard link.
  `harness --view-session <id>` reads the replayed history as JSONL without
  provider credentials or a writer lease, including while a writer is active.
  A view is a snapshot of completed records at read time.
  New transcripts start with a versioned metadata record containing the canonical
  workspace, directory creation identity, optional Git metadata directory identity,
  and creation time. This record is excluded from model history and preserved by
  clear, compaction, rollback and rename. Resume and `/resume` require the same
  workspace and repository; replacing a directory or changing repositories also
  requires a new binding. A session another process is writing returns
   `session is already open for writing` (`ErrSessionBusy`) and is left alone:
   ordinary `--resume` and `/resume` do not take that lock. `harness --resume <id> --steal`
   and `/steal <id>` ask for confirmation, then take the lock and reopen the session
   as one handoff. A live owner is signalled only when its host, heartbeat and process
   identity all match and the heartbeat is stale. If identity cannot be proven,
   including on Windows when the PID cannot be opened or its creation time and image
   path cannot be read, or the recorded identity does not match, the steal is refused.
   A PID that no longer exists is a dead owner and may be recovered without a signal.
   RPC `resume_session` refuses the same way; `steal_session` is the confirmed handoff.
   Symlink aliases of the same directory are accepted.
  To adopt an older transcript without metadata or move one deliberately, run
  `harness --root <project> --resume <id> --rebind-session`. This preserves history
  and creation time when available; it does not grant workspace trust. Unsupported
  schema versions are rejected even with this flag. On filesystems without a
  directory creation identity, each reopen requires explicit rebind; new sessions
  and the read-only viewer remain available. Repository metadata is discovered
  from `.git` directories/files without running Git or loading its configuration.
- REPL commands: `/usage` shows session token totals, `/clear` drops
  the conversation context (a marker is written to the session file;
  `--resume` starts from the clear point, older messages stay in the
  file). `/plugins` lists the discovered plugins with their tools
  and state. `/sessions` lists the sessions (newest first, with message
  count and the first user message as a preview; the current one is
  marked `*`); `/resume <id>` switches to one and reports the busy error when
   another process holds it, `/steal <id>` asks and then takes a proven stalled
   or dead owner, `/rename <old> <new>`
  renames one (transcript, audit sidecar and pre-compact archives
  follow). `/model` lists the configured models; `/model <provider>/<model>`
  (or a bare model id) switches model mid-session — the client is
  rebuilt from that model's config (api, baseUrl, maxTokens,
  contextWindow, image input) and history replays through the new
  API on the next turn. The switch lasts for the session only; it
  does not rewrite the config file.
- Compaction: `/compact` replaces narration with a 6-section brief alongside
  program-preserved source facts and the latest task; the original transcript is
  saved to `<session>-pre-compact-<time>.jsonl`. It also triggers
  inside the tool loop, before the next model call, when measured
  usage plus new content reaches 70% of the window (`-context-limit`, default: the model's `contextWindow`,
  else 128000). Summary requests use the same bounded retry policy as normal
  steps (four attempts by default, with backoff and `Retry-After` support).
  Retries are labelled `Compaction` in `step_retry` events. Failed or truncated
  summaries never replace history. If retries are exhausted, the turn stops
  with an explicit failure report rather than an empty answer.
  Source facts retain original human instructions, assistant statements and user
  answers, latest verification attempts (including failures), unresolved errors,
  invocation IDs, job observations and image/output/transcript references.
  They are historical data: permissions and current verification still use live
  policy and fresh evidence. A baseline before the latest task lets rollback
  remove that task's facts; older compressed turns remain in transcript archives.
  Summary requests have a separate 128 KiB input cap and reserve the configured
  output limit within the context window. Optional narration uses a bounded
  recent suffix with explicit omissions and head/tail excerpts. The summary
  can omit older optional facts to fit its input budget. Mandatory facts,
  including unresolved failures, are preserved. If they exceed the byte,
  entry, or context limits, compaction fails without replacing history.
- Context checkpoints: normal requests use one fixed 32 KiB cap per tool
  result and never prune by age. At the compaction boundary, the harness first
  tries replacing older successful tool outputs with a fixed placeholder, keeping
  failed observations and the latest tool batch. It commits that view only if it brings estimated/projected
  input below 60% of the trigger budget; otherwise it generates a summary.
  Both paths archive the full transcript and persist the replacement atomically,
  so resume retains the checkpoint. This trades larger input between checkpoints
  for fewer prefix rewrites; it does not guarantee lower token cost or cache hits.
- Turn context: mode and `before_agent_start` plugin state are appended after
  each task as a persisted snapshot. Historical snapshots remain unchanged;
  the newest snapshot describes current state, including when no plugin adds
  content. Explicit `context` hooks can still rewrite the request transiently.
- Step budget: steps are unlimited by default (like Claude Code,
  Codex, Gemini, Qwen, OpenCode and Pi): a turn runs until the model
  stops calling tools, an error ends it, or the task is blocked
  (three failed attempts without proven progress). The bounds are auto-compaction
  at 70% of the window plus the doom-loop guard (5 consecutive
  identical calls) and the stop-hook veto cap (8). Pass
  `-max-steps N` to opt into a hard cap; when a cap hits, the model
  gets one final tool-less call (60s) to summarise what was done,
  then the turn ends `max_turns` — the same shape as OpenCode's
  final step. A turn that ends `blocked` gets the same treatment:
  one tool-less call (60s) whose prompt names the blocked reason
  and the last errors, so the settled text is a short summary of
  what was verified, what failed and what is left — a blocked turn
  used to settle with an empty text and no report at all. Grace requests use
  the same context checkpoint guard. A failed final attempt adds no assistant
  message, but any completed checkpoint and reported usage are retained.
- Repeat failures are counted by the effective tool name and canonical arguments,
  within one turn, including asynchronous Bash results. The second failure warns;
  after three, the same invocation needs proven changed state to run again.
  At most two changed-state probes are admitted per invocation and eight per turn.
  Returning to a known failed or previously admitted version cannot open a probe.
  Invalid input, policy denial, environment, transient and verification failures
  are distinguished; edits do not repair an unchanged input or policy rejection.
  Denials carry `repeated_tool_failure_denied`, separately from a user refusing the
  doom-loop continuation prompt (`repeated_tool_call_denied`).
- When a tool batch leaves three failed attempts without proven state or result
  progress, the turn settles `blocked`, with one tool-free grace report. Successful reads, unchanged writes
  and unrelated commands returning success do not reset this count. A successful
  rerun of a previously failing invocation, or a proven workspace change after
  a successful foreground mutating tool, does. Failed commands' own generated output files
  are included in their post-failure baseline and do not look like a later repair.
  Completed job failures are accounted before accepting state progress.
  Background results use the original effective invocation, even when collected
  by a later turn. Counts reset for each turn; successful probes do not refund
  that turn's probe allowance. Model request retries remain independent.
- State evidence uses the bounded workspace capture described under verification,
  including its `verification.exclude` settings. Unknown, unstable, unsupported
  or oversized captures cannot prove a repair. Capture is only attempted during
  failure handling or repair checks, at most 64 times per turn and five seconds
  per capture; normal successful turns need no snapshots for the failure guard.
  If evidence is unavailable or the probe allowance is exhausted, report the
  blocker and start a new turn after choosing a different approach or fixing
  the capture scope.
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
  running after the turn settles (and after quit, orphaned) until it exits on its
  own or is stopped, so a dev server started in one turn is still serving in the
  next. On resume after a harness crash, verifiable live background jobs are safely
  adopted while exited
  processes or reused PIDs are reported as `unknown` without replaying commands;
  use `/jobs stop <id>` to explicitly terminate an active/adopted job. Output files
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
turn, ahead of the reply. Thinking is separate from the answer. Anthropic replay blocks are saved in
the session; token totals remain provider-reported. Usage lines stay in the timeline with thousands
separators. The approval dialog is a full-width panel:
`△ Approval required`, the tool summary, `[y] Allow once` on a solid
blue background, `[n] Deny`, `esc = deny` (single key, Esc = deny).
The composer is a `┃` input line with a meta line: left is the mode
chip · model · api, right is the workspace git branch and the session
totals (`3 turns · 12,345 tokens`). Themes:
`HARNESS_TUI_THEME=default|terminal|catppuccin-mocha`; `terminal`
paints no raised background, `catppuccin-mocha` is the Catppuccin
Mocha palette.
