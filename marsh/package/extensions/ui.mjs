// Display only. No job spawning, cancellation, retries, inference or file writes.
const STATUS = "pix";
const clean = value => String(value ?? "").replace(/[\x00-\x1f\x7f]/g, "");
const elapsed = ms => `${Math.floor(ms / 60_000)}:${String(Math.floor(ms / 1000) % 60).padStart(2, "0")}`;

export default function (pi) {
  let ui;
  let active = false;
  let started = 0;
  let lastActivity = Date.now();
  let reply;
  let timer;
  const tools = new Map();

  const phase = () => tools.size ? `running ${[...tools.values()].map(clean).join(", ")}` : "awaiting model";
  const render = () => {
    if (!ui || !active) return;
    const quiet = !tools.size && Date.now() - lastActivity > 60_000 ? " · quiet stream" : "";
    ui.setWorkingMessage?.(`${phase()} · ${elapsed(Date.now() - started)}${quiet}`);
  };
  const stop = () => {
    active = false;
    tools.clear();
    if (timer) clearInterval(timer);
    timer = undefined;
    ui?.setWorkingMessage?.();
  };
  const report = ctx => {
    const model = ctx.model ? `${ctx.model.provider}/${ctx.model.id}` : "not selected";
    const usage = ctx.getContextUsage?.();
    return [
      `State: ${active ? `${phase()} · ${elapsed(Date.now() - started)}` : "idle"}`,
      `Model: ${clean(model)}`,
      ...(usage?.tokens != null ? [`Context: ~${usage.tokens} tokens`] : []),
      ...(reply ? [`Last reply: ${reply}`] : []),
      "Execution: marsh owns jobs/cancellation; this UI never cancels or retries.",
    ].join("\n");
  };

  pi.on("session_start", (_event, ctx) => {
    // RPC/print mode must not acquire timers or require terminal dialogs.
    if (ctx.hasUI && ctx.mode === "tui") {
      ui = ctx.ui;
      ui.setStatus?.(STATUS, "Pix UI");
    }
  });
  pi.on("agent_start", (_event, ctx) => {
    if (!ui && ctx.hasUI && ctx.mode === "tui") ui = ctx.ui;
    active = true;
    started = lastActivity = Date.now();
    tools.clear();
    if (ui && !timer) {
      timer = setInterval(render, 1000);
      timer.unref?.();
    }
    render();
  });
  pi.on("tool_execution_start", event => {
    tools.set(event.toolCallId, event.toolName);
    lastActivity = Date.now();
    render();
  });
  pi.on("tool_execution_update", () => { lastActivity = Date.now(); });
  pi.on("tool_execution_end", event => {
    tools.delete(event.toolCallId);
    lastActivity = Date.now();
    render();
  });
  pi.on("message_update", () => { lastActivity = Date.now(); });
  pi.on("message_end", event => {
    if (event.message.role === "assistant" && event.message.content?.some(part => part.type === "text" && part.text)) {
      // Explicit UTC avoids pretending the VM clock is the Mac's local timezone.
      reply = new Date().toISOString().slice(11, 16) + "Z";
      ui?.setStatus?.(STATUS, `reply ${reply}`);
    }
  });
  pi.on("agent_end", stop);
  pi.on("session_shutdown", () => {
    stop();
    ui?.setStatus?.(STATUS, undefined);
    ui = undefined;
  });
  pi.registerCommand("pix-status", {
    description: "Show Pix phase/model/context without a model call or lifecycle changes",
    handler: async (_args, ctx) => {
      ctx.ui.notify(report(ctx), "info");
    },
  });
}
