// plan — plan mode state + the /plan and /execute commands. The
// pilot plugin for Phase 3 (commands + hooks + modes): it proves a
// plugin can register slash commands and switch the agent's tool
// mode.
//
// State is one JSON file, plan.json, in the plugin's own data dir
// (harness.readFile / harness.writeFile are jailed there: the plugin
// cannot read or write anywhere else, and cannot run bash).
//
// Mode switches happen only through the user's /plan and /execute
// commands below. The plan tools never call setMode: the model
// manages plan state, the user decides when to plan and when to
// execute. A plan is a reference for the model, not a contract —
// plan mode enforces read-only by removing write/edit/bash from the
// offered tools; it does not force the model to follow the plan
// once it is back in default mode.

function loadPlan() {
  try {
    const raw = harness.readFile("plan.json");
    const plan = JSON.parse(raw);
    if (!plan || !Array.isArray(plan.steps)) return null;
    return plan;
  } catch (e) {
    // No plan file yet (or unreadable): no plan.
    return null;
  }
}

function savePlan(plan) {
  harness.writeFile("plan.json", JSON.stringify(plan, null, 2));
}

function formatPlan(plan) {
  const lines = ["Plan: " + plan.title];
  plan.steps.forEach((s, i) => {
    lines.push("#" + (i + 1) + (s.done ? " [done] " : " [open] ") + s.text);
  });
  return lines.join("\n");
}

harness.registerTool(
  "plan_create",
  "Create (or replace) the plan: a title and an ordered list of steps.",
  {
    type: "object",
    properties: {
      title: { type: "string", description: "The plan title" },
      steps: {
        type: "array",
        items: { type: "string" },
        description: "The plan steps, in order",
      },
    },
    required: ["title", "steps"],
  },
  (args) => {
    const steps = Array.isArray(args.steps) ? args.steps : [];
    const had = loadPlan() !== null;
    const plan = {
      title: String(args.title || "Untitled plan"),
      steps: steps.map((s) => ({ text: String(s), done: false })),
    };
    savePlan(plan);
    return (had ? "Replaced the previous plan.\n" : "") + formatPlan(plan);
  },
);

harness.registerTool(
  "plan_step",
  "Change the text of one plan step, by its 1-based number from plan_show.",
  {
    type: "object",
    properties: {
      index: { type: "integer", description: "The step number (1-based)" },
      text: { type: "string", description: "The new step text" },
    },
    required: ["index", "text"],
  },
  (args) => {
    const plan = loadPlan();
    if (!plan) return "Error: no plan yet. Use plan_create first.";
    const i = Number(args.index) - 1;
    if (!plan.steps[i]) return "Error: no step #" + args.index + ".";
    plan.steps[i].text = String(args.text);
    savePlan(plan);
    return formatPlan(plan);
  },
);

harness.registerTool(
  "plan_done",
  "Mark one plan step done, by its 1-based number from plan_show.",
  {
    type: "object",
    properties: {
      index: { type: "integer", description: "The step number (1-based)" },
    },
    required: ["index"],
  },
  (args) => {
    const plan = loadPlan();
    if (!plan) return "Error: no plan yet. Use plan_create first.";
    const i = Number(args.index) - 1;
    if (!plan.steps[i]) return "Error: no step #" + args.index + ".";
    if (plan.steps[i].done)
      return "Step #" + args.index + " is already done: " + plan.steps[i].text;
    plan.steps[i].done = true;
    savePlan(plan);
    return formatPlan(plan);
  },
);

harness.registerTool(
  "plan_show",
  "Show the current plan with its steps and their state.",
  { type: "object", properties: {} },
  () => {
    const plan = loadPlan();
    if (!plan) return "No plan yet.";
    return formatPlan(plan);
  },
);

harness.registerCommand(
  "plan",
  "Switch to plan mode: read-only, write a plan first.",
  () => {
    if (harness.getMode() === "plan") return "Already in plan mode.";
    harness.setMode("plan");
    return "Plan mode: read-only. Explore and write a plan with plan_create.";
  },
);

harness.registerCommand(
  "execute",
  "Leave plan mode and go back to default mode to execute the plan.",
  () => {
    if (harness.getMode() === "default") return "Already in default mode.";
    harness.setMode("default");
    return "Back in default mode.";
  },
);

// session_before_compact: ask the summary to keep the plan, so a
// compacted session does not lose what the user is working towards.
// No plan means no contribution (undefined). The "[plugin plan] "
// prefix is added by the harness when it joins instructions, so
// this returns the bare text.
harness.on("session_before_compact", (event) => {
  const plan = loadPlan();
  if (!plan) return undefined;
  const open = plan.steps.filter((s) => !s.done);
  return (
    "Keep the current plan in the summary — title: \"" +
    plan.title +
    "\", open steps: " +
    (open.length ? open.map((s) => s.text).join("; ") : "none")
  );
});
