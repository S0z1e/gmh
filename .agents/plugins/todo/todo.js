// todo — a personal todo list. The pilot plugin for the harness
// plugin system: it proves discovery, tool registration, the jailed
// store, and state that persists between turns.
//
// State is one JSON file, todos.json, in the plugin's own data dir
// (harness.readFile / harness.writeFile are jailed there: the
// plugin cannot read or write anywhere else, and cannot run bash).
// A missing state file means an empty list.

function loadTodos() {
  try {
    const raw = harness.readFile("todos.json");
    const items = JSON.parse(raw);
    return Array.isArray(items) ? items : [];
  } catch (e) {
    // No state file yet (or unreadable): start empty.
    return [];
  }
}

function saveTodos(items) {
  harness.writeFile("todos.json", JSON.stringify(items, null, 2));
}

function nextId(items) {
  return items.reduce((m, t) => Math.max(m, t.id || 0), 0) + 1;
}

harness.registerTool(
  "todo_add",
  "Add a todo item to the personal todo list.",
  {
    type: "object",
    properties: {
      text: { type: "string", description: "The todo text" },
    },
    required: ["text"],
  },
  (args) => {
    const text = String(args.text || "").trim();
    if (!text) return "Error: todo text is empty.";
    const items = loadTodos();
    const id = nextId(items);
    items.push({ id, text, done: false });
    saveTodos(items);
    return "Added todo #" + id + ": " + text;
  },
);

harness.registerTool(
  "todo_list",
  "List the personal todo list: open items first, done items marked [done].",
  {
    type: "object",
    properties: {
      all: {
        type: "boolean",
        description: "Include done items (default true)",
      },
    },
  },
  (args) => {
    const items = loadTodos();
    if (items.length === 0) return "No todos.";
    const shown = args.all === false ? items.filter((t) => !t.done) : items;
    if (shown.length === 0) return "No open todos.";
    return shown
      .map((t) => "#" + t.id + (t.done ? " [done] " : " [open] ") + t.text)
      .join("\n");
  },
);

harness.registerTool(
  "todo_done",
  "Mark a todo item done, by the id from todo_list.",
  {
    type: "object",
    properties: {
      id: { type: "integer", description: "The todo id from todo_list" },
    },
    required: ["id"],
  },
  (args) => {
    const id = Number(args.id);
    const items = loadTodos();
    const item = items.find((t) => t.id === id);
    if (!item) return "Error: no todo #" + args.id + ".";
    if (item.done) return "Todo #" + id + " is already done: " + item.text;
    item.done = true;
    saveTodos(items);
    return "Done todo #" + id + ": " + item.text;
  },
);

// before_agent_start: inject the open todos into every turn, so the
// model knows what is pending without calling todo_list first. No
// open todos means no contribution (undefined). The "[plugin todo] "
// prefix is added by the harness when it joins contributions, so
// this returns the bare text.
harness.on("before_agent_start", () => {
  const open = loadTodos().filter((t) => !t.done);
  if (open.length === 0) return undefined;
  return (
    "Open todos (" +
    open.length +
    "): " +
    open.map((t) => "#" + t.id + " " + t.text).join("; ")
  );
});
