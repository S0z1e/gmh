# Plugins (JavaScript, via moejs)

Plugins extend the agent with new tools written in JavaScript (or
TypeScript, types stripped). One directory per plugin under
`.agents/plugins/<name>/`, with a `plugin.json` and one entry file:

```json
{"name": "todo", "description": "A personal todo list.", "version": "1.0.0", "entry": "todo.js"}
```

The entry registers tools at its top level through the `harness`
global; the model then sees them like the eight built-ins:

```js
harness.registerTool("todo_add", "Add a todo item.", {
  type: "object",
  properties: { text: { type: "string" } },
  required: ["text"],
}, (args) => {
  // ...return the observation as a string
});
```

Host API: `harness.registerTool(name, description, parameters,
handler)`; `harness.log(...)` (one line on stderr);
`harness.readFile(path)` / `harness.writeFile(path, content)` — a
file store jailed to the plugin's own data dir,
`.agents/plugins/<name>/data/` (paths must stay inside it;
`../escape` and absolute paths are refused, and plugins cannot see
each other's data). No bash, no network, no arbitrary filesystem in
Phase 1. Phase 3 adds commands, hooks and modes (below); Phase 4
adds the lifecycle hooks (also below).

A plugin tool runs JS only, inside its own runtime (16 MiB memory
limit, 10 s call timeout with interrupt — a timed-out or
memory-limited call leaves the runtime usable for the next call),
so it needs no approval in suggest mode. A bad plugin is skipped
with a warning on stderr and never fails startup; a missing
plugins directory loads nothing, no error. Kill switch:
`HARNESS_PLUGINS=0` (or `false`) disables plugin loading entirely.

Disable one plugin in the config file (global or project; a project
section replaces the global one):

```json
{"plugins": {"disabled": ["todo"]}}
```

A disabled plugin is skipped entirely at load — its entry is never
compiled, no runtime is created — and its tools never reach the
model. A disabled name that matches no plugin warns at load, so a
typo is visible. `/plugins` lists every discovered plugin with its
tools and state (`loaded` / `disabled` / `skipped` with the reason),
and RPC `list_plugins` returns the same information. There is no hot
reload and no enable/disable command: edit the config file and
restart. The harness never rewrites the config file.

## Commands

A plugin can register slash commands the user runs from the REPL
(or the GUI, via the RPC `command` request):

```js
harness.registerCommand("plan", "Switch to plan mode.", (args) => {
  // args is the raw text after "/plan", unparsed.
  return "Plan mode: read-only.";
});
```

Commands register at load time only (like tools: a handler that
registers one after load fails). The name must be lowercase
alphanumeric with underscores, and must not collide with a built-in
slash command (`image`, `sessions`, `plugins`, `resume`, `rename`,
`model`, `quit`, `exit`, `usage`, `jobs`, `clear`, `compact`,
`skill`). The handler returns the output text (any other value is
JSON-encoded); a throw or a call past the 10 s timeout becomes
`[error]`. An unknown `/foo` is still a task for the model, not an
error.

## Hooks

A plugin can watch and gate tool calls (at most one handler per
event per plugin, registered at load time only):

```js
harness.on("tool_call", (input) => {
  // input: {tool, args}. Return undefined (or true) to allow,
  // {deny: "reason"} to deny, or {args: {...}} to rewrite the call.
});
harness.on("tool_result", (input) => {
  // input: {tool, args, result, error}. Return a string to replace
  // the observation the model sees, or undefined to keep it.
});
```

`tool_call` hooks run after the tool-start event and before the
approval prompt, in plugin load order; the first denial stops the
chain, and arg rewrites chain on. A denied call never prompts: the
model gets the observation `Denied by plugin <name>: <reason>` and a
tool-end event with an error, and the denial is audited like a
refused approval. A hook that throws or times out **denies the
call** (fail closed). `tool_result` hooks run after the tool
dispatches (bash never reaches them — it has its own path) and
before the result enters history; a hook that throws, times out, or
returns a non-string keeps the original result and warns on stderr
(fail open). Hooks never bypass approval: anything they let through
still prompts in suggest mode.

## Lifecycle hooks

Four more hooks, all registered with `harness.on` at load time
only, at most one per event per plugin. Unlike `tool_call`, all
four **fail open**: a hook that throws, times out, or returns an
unusable value is skipped with a stderr warning and the turn,
compact, or session switch carries on without it.

`before_agent_start` — per-turn context injection. Runs once per
turn, before the model call, in plugin load order; the
contributions are concatenated into one system message, each
prefixed `"[plugin <name>] "`. The todo pilot uses it to inject
the open todos, so the model knows what is pending without calling
`todo_list` first.

```js
harness.on("before_agent_start", (input) => {
  // input: {session, mode}. Return a string to inject, or
  // undefined (or an empty string) for nothing.
  return "Open todos (2): #1 buy milk; #2 call mom";
});
```

`context` — rewrite the turn's starting messages. Runs once per
turn, after `before_agent_start` and before the model call, in
load order (each hook sees the previous one's rewrite). The input
is `{session, mode, messages}` — the turn's starting history with
the system message excluded (the harness prepends its own system
prompt itself). Return the modified list, or undefined to keep the
current one. The rewrite is in-memory only: the session file
keeps the true history, so a rewrite never persists. The returned
list is validated (an array of messages with roles `user`,
`assistant` or `tool` — a `system` message is rejected, since the
harness owns the system prompt); an invalid return keeps the
original. Image bytes never travel as JSON, so images in a
rewritten list are restored from the original history by path —
a hook cannot inject image bytes it never had.

`session_before_compact` — extra instructions for the summary.
Runs when a session is compacted (manual `/compact` and
auto-compact share the one insertion point), in load order; the
instructions are appended to the compact prompt after section 6
and before the conversation, each prefixed `"[plugin <name>] "`.
The plan pilot uses it to ask the summary to keep the current
plan. It is append-only: a plugin steers what the summary keeps,
it cannot replace the summary.

```js
harness.on("session_before_compact", (input) => {
  // input: {session, messages: count} — the count, not the list.
  return 'Keep the current plan in the summary — title: "Demo".';
});
```

`session_start` / `session_switch` — notifications when a session
becomes current (`NewSession`, `LoadSession`, `ResumeSession`;
the initial session at startup fires nothing, plugins are not
loaded yet). `session_start` fires every time, with `{session}`;
`session_switch` fires only when the id changes, with `{session,
previous}`. `/clear` (`ResetSession`) fires neither. Return values
are ignored; a failing hook warns and never fails the switch.

| Hook | Input | Return | On failure |
| --- | --- | --- | --- |
| `tool_call` | `{tool, args}` | allow / `{deny}` / `{args}` | **deny the call** (fail closed) |
| `tool_result` | `{tool, args, result, error}` | replacement string | keep the original result |
| `before_agent_start` | `{session, mode}` | string to inject | skip the contribution |
| `context` | `{session, mode, messages}` | rewritten list | keep the original history |
| `session_before_compact` | `{session, messages: count}` | instruction string | skip the instruction |
| `session_start` | `{session}` | ignored | warn only |
| `session_switch` | `{session, previous}` | ignored | warn only |

## Modes (plan mode)

The loop's tool set has two modes: `default` (all tools) and `plan`
(read-only — `write`, `edit` and `bash` are removed; plugin tools
stay, because a JS tool is jailed to its own data dir and needs no
approval). A plugin reads the mode with `harness.getMode()` and
switches it with `harness.setMode("plan" | "default")`; an unknown
mode is an error. Modes switch only between turns, and a switch
queues a one-line system reminder for the next turn so the model is
told the tools changed. The mode is session-only: a new or resumed
session starts in `default`, and it is never written to the config
file. The TUI shows a `plan` chip next to the approval-mode chip
while plan mode is on; RPC `get_state` and `agent_settled` carry
`tool_mode`.

The shipped pilot is `.agents/plugins/plan/` — tools
`plan_create` / `plan_step` / `plan_done` / `plan_show` (state in
`data/plan.json`, steps numbered from 1) and commands `/plan` +
`/execute`. The flow: `/plan` → explore (read/grep/glob/webfetch)
and write the plan → review it → `/execute` → the model executes
it. Plan approval is the user's next message ("go ahead"); `/execute`
only switches the mode back. A plan is a reference for the model,
not a contract — plan mode enforces read-only by removing the
mutating tools; it does not force the model to follow the plan once
it is back in default mode.

Cost: moejs is the one non-stdlib dependency outside the TUI, and it
adds ~8 MB to the stripped binary (core harness ~8 MB → ~16 MB).
The shipped pilot is `.agents/plugins/todo/` — `todo_add` /
`todo_list` / `todo_done`, state in its `data/todos.json`.
